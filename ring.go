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

// ringFlight is one in-progress ring; holders await done and read outcome.
type ringFlight struct {
	done    chan struct{}
	outcome ringOutcome
	waiters int
}

// awaitRing joins (or starts) the app's single outstanding ring and blocks
// until it resolves, the waiter cap rejects us, or the client goes away.
func (dp *dataPlane) awaitRing(ctx context.Context, appID, sockPath string) ringOutcome {
	dp.mu.Lock()
	f := dp.flights[appID]
	if f == nil {
		f = &ringFlight{done: make(chan struct{})}
		dp.flights[appID] = f
		go dp.runRing(appID, sockPath, f)
	}
	if f.waiters >= dp.waiterCap {
		dp.mu.Unlock()
		return ringOutcome{kind: ringOverflow}
	}
	f.waiters++
	dp.mu.Unlock()

	select {
	case <-f.done:
		return f.outcome
	case <-ctx.Done():
		dp.mu.Lock()
		f.waiters--
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
	dp.mu.Lock()
	delete(dp.flights, appID)
	dp.mu.Unlock()
	f.outcome = outcome
	close(f.done)
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
