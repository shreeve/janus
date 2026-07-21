package janus

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	"go.uber.org/zap"
)

// The ring (docs/20260719-002000-pool-protocol.md "The ring (Janus side)").
//
// A doorbell upstream is never forwarded the client's request. Janus sends
// its own bodyless GET /ring on a connection to the doorbell socket while the
// client's request holds untouched — the body is never solicited early, so
// the eventual delivery to a fresh worker is a first delivery. At most one
// ring is outstanding per app; every holder for that app awaits the same
// outcome. Doorbell responses never count toward upstream health.

type ringKind int

const (
	ringWoke       ringKind = iota // 204: pool is up; re-resolve and proxy
	ringBootError                  // 503 from the doorbell: forward the boot error
	ringFailed                     // connection error / timeout / EOF / bogus status
	ringOverflow                   // waiter cap reached; this request never held
	ringClientGone                 // client disconnected during the hold
)

type ringOutcome struct {
	kind        ringKind
	body        []byte // boot-error body (bounded), shared by all holders
	contentType string
	reason      string
}

// ringClass separates holder budgets: ordinary client HTTP requests get
// the 64-holder budget; hub bridge POSTs get a separate, lower-priority
// budget of 16 per app, so bridge observation can never consume client
// capacity. Both classes share the app's ring single-flight and
// three-ring bound.
type ringClass int

const (
	ringClassClient ringClass = iota
	ringClassBridge
)

type ringClassKey struct{}

func withRingClass(ctx context.Context, cl ringClass) context.Context {
	return context.WithValue(ctx, ringClassKey{}, cl)
}

func ringClassOf(ctx context.Context) ringClass {
	cl, _ := ctx.Value(ringClassKey{}).(ringClass)
	return cl
}

// ringFlight is one in-progress ring; holders await done and read outcome.
type ringFlight struct {
	done          chan struct{}
	outcome       ringOutcome
	waiters       int // client-class holders
	bridgeWaiters int // bridge-class holders (separate budget)
}

// awaitRing joins (or starts) the app's single outstanding ring and blocks
// until it resolves, the waiter cap rejects us, or the client goes away.
func (dp *dataPlane) awaitRing(ctx context.Context, appID, sockPath string) ringOutcome {
	class := ringClassOf(ctx)
	dp.mu.Lock()
	f := dp.flights[appID]
	if f == nil {
		f = &ringFlight{done: make(chan struct{})}
		dp.flights[appID] = f
		go dp.runRing(appID, sockPath, f)
	}
	if class == ringClassBridge {
		if f.bridgeWaiters >= hubBridgeWaiterCap {
			dp.mu.Unlock()
			return ringOutcome{kind: ringOverflow}
		}
		f.bridgeWaiters++
	} else {
		if f.waiters >= dp.waiterCap {
			dp.mu.Unlock()
			return ringOutcome{kind: ringOverflow}
		}
		f.waiters++
	}
	dp.mu.Unlock()

	select {
	case <-f.done:
		return f.outcome
	case <-ctx.Done():
		dp.mu.Lock()
		if class == ringClassBridge {
			f.bridgeWaiters--
		} else {
			f.waiters--
		}
		dp.mu.Unlock()
		return ringOutcome{kind: ringClientGone}
	}
}

func (dp *dataPlane) runRing(appID, sockPath string, f *ringFlight) {
	dp.logger.Info("janus ringing doorbell",
		zap.String("app", appID),
		zap.String("doorbell", sockPath),
	)
	outcome := dp.doRing(appID, sockPath)
	if outcome.kind == ringFailed {
		dp.logger.Warn("janus ring failed",
			zap.String("app", appID),
			zap.String("doorbell", sockPath),
			zap.String("reason", outcome.reason),
		)
	}
	// Publish the outcome, then retire the flight and release its holders
	// in one critical section. The outcome write happens-before close(done)
	// (holders read it only after <-done), and a new arrival can never find
	// the flight after its done channel closes — deleting first and closing
	// later would open a gap where a second flight starts while up to a
	// full waiter cap of holders is still parked on this one.
	f.outcome = outcome
	dp.mu.Lock()
	delete(dp.flights, appID)
	close(f.done)
	dp.mu.Unlock()
}

// doRing sends GET /ring (no body, Host = app id) on its own connection to
// the doorbell socket, bounded by the ring timeout. It deliberately bypasses
// dp.transport and dp.state: doorbells are excluded from health accounting.
func (dp *dataPlane) doRing(appID, sockPath string) ringOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), dp.ringTimeout)
	defer cancel()

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		},
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+appID+"/ring", nil)
	if err != nil {
		return ringOutcome{kind: ringFailed, reason: err.Error()}
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return ringOutcome{kind: ringFailed, reason: err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBootErrorBody))

	switch resp.StatusCode {
	case http.StatusNoContent:
		return ringOutcome{kind: ringWoke}
	case http.StatusServiceUnavailable:
		return ringOutcome{
			kind:        ringBootError,
			body:        body,
			contentType: resp.Header.Get("Content-Type"),
		}
	default:
		return ringOutcome{
			kind:   ringFailed,
			reason: fmt.Sprintf("doorbell answered %d (want 204 or 503)", resp.StatusCode),
		}
	}
}
