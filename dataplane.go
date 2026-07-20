package janus

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Data-plane knobs (docs/20260719-002000-pool-protocol.md "Defaults").
//
// The doorbell hold must never race a request-read timeout. Caddy's site
// servers default to ReadTimeout 0 (disabled) with only ReadHeaderTimeout
// (1m, elapsed before the handler runs), so a held request idles safely for
// the full ring timeout.
const (
	// defaultRingTimeout bounds one ring: tenant hold cap (~15s) + margin.
	defaultRingTimeout = 17 * time.Second

	// defaultWaiterCap bounds holders per app; overflow → immediate 503.
	defaultWaiterCap = 64

	// defaultMaxRings bounds rings per held request; past it → 503.
	defaultMaxRings = 3

	// defaultUnhealthyWindow is how long a failed upstream stays deselected.
	defaultUnhealthyWindow = 2 * time.Second

	// upstreamDialTimeout bounds one unix-socket dial.
	upstreamDialTimeout = 3 * time.Second

	// retryAfter accompanies every data-plane 503.
	retryAfter = "1"

	// maxBootErrorBody bounds a doorbell 503 body forwarded to holders.
	maxBootErrorBody = 64 << 10
)

// upstreamState is per-socket passive health and least-conn accounting.
// Doorbell sockets never get an entry: they are excluded from health.
type upstreamState struct {
	inflight       int
	unhealthyUntil time.Time
}

// dataPlane routes admitted requests: host → registry → upstream unix socket.
type dataPlane struct {
	registry *appRegistry
	logger   *zap.Logger

	ringTimeout     time.Duration
	waiterCap       int
	maxRings        int
	unhealthyWindow time.Duration

	// transport pools keep-alive connections per socket path (the socket
	// path is hex-encoded into the synthetic URL host, so the transport's
	// own per-host pooling applies and idle conns expire normally).
	transport *http.Transport

	mu      sync.Mutex
	state   map[string]*upstreamState // socket path → health + inflight
	flights map[string]*ringFlight    // app id → the one outstanding ring
}

func newDataPlane(reg *appRegistry, logger *zap.Logger) *dataPlane {
	if logger == nil {
		logger = zap.NewNop()
	}
	dp := &dataPlane{
		registry:        reg,
		logger:          logger,
		ringTimeout:     defaultRingTimeout,
		waiterCap:       defaultWaiterCap,
		maxRings:        defaultMaxRings,
		unhealthyWindow: defaultUnhealthyWindow,
		state:           map[string]*upstreamState{},
		flights:         map[string]*ringFlight{},
	}
	dp.transport = &http.Transport{
		DialContext:         dp.dialUpstream,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
	}
	return dp
}

// serve applies the data-plane decision table, top to bottom.
func (dp *dataPlane) serve(w http.ResponseWriter, r *http.Request) error {
	host := normalizeHostHeader(r.Host)
	rec, ok := dp.registry.resolveHost(host)
	if !ok {
		return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("janus: unknown host %q", host))
	}

	rings := 0
	for {
		if len(rec.Upstreams) == 0 {
			return dp.unavailable(w, rec.ID, "upstreams empty (down on purpose)")
		}
		bell, isBell := doorbellOf(rec)
		if !isBell {
			return dp.proxyWorkers(w, r, rec)
		}
		if rings >= dp.maxRings {
			return dp.unavailable(w, rec.ID, fmt.Sprintf("ring retry cap (%d) reached", dp.maxRings))
		}
		rings++
		out := dp.awaitRing(r.Context(), rec.ID, bell.Path)
		switch out.kind {
		case ringWoke:
			// 204 is empty and advisory; trust only our own registry.
			rec, ok = dp.registry.resolveHost(host)
			if !ok {
				return caddyhttp.Error(http.StatusNotFound,
					fmt.Errorf("janus: host %q vanished during ring", host))
			}
		case ringBootError:
			// Forward the tenant's 503 verbatim; it carries the boot error.
			if out.contentType != "" {
				w.Header().Set("Content-Type", out.contentType)
			}
			w.Header().Set("Retry-After", retryAfter)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, err := w.Write(out.body)
			return err
		case ringOverflow:
			return dp.unavailable(w, rec.ID, "ring waiter cap reached")
		case ringClientGone:
			// Client disconnected during the hold; abandon this holder only.
			return nil
		default: // ringFailed: connection error / timeout / EOF / bogus status
			return dp.unavailable(w, rec.ID, "ring failed: "+out.reason)
		}
	}
}

// doorbellOf reports the doorbell upstream. The registry guarantees a
// doorbell entry is the only entry in the list.
func doorbellOf(rec AppRecord) (Upstream, bool) {
	if len(rec.Upstreams) == 1 && rec.Upstreams[0].Doorbell {
		return rec.Upstreams[0], true
	}
	return Upstream{}, false
}

// normalizeHostHeader strips any port and lowercases for registry lookup.
func normalizeHostHeader(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func (dp *dataPlane) unavailable(w http.ResponseWriter, appID, reason string) error {
	dp.logger.Warn("janus data plane unavailable",
		zap.String("app", appID),
		zap.String("reason", reason),
	)
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	return nil
}

// --- proxy ------------------------------------------------------------------

// dialError marks a failure to establish the unix connection at all — the
// request body was never touched, so trying another upstream is safe.
type dialError struct {
	path string
	err  error
}

func (e *dialError) Error() string { return fmt.Sprintf("dial %s: %v", e.path, e.err) }
func (e *dialError) Unwrap() error { return e.err }

// Worker-marked 503s (docs/20260719-002000-pool-protocol.md "Passive
// health"): a worker at its concurrency cap answers 503 with
// Rip-Worker-Busy: 1 (drains: Rip-Worker-Draining: 1). Those are protocol
// flow control, not failures — they never count toward health, and when the
// request is replayable (no body was sent) Janus immediately tries the next
// upstream instead of forwarding the bounce to the client.
const (
	workerBusyHeader     = "Rip-Worker-Busy"
	workerDrainingHeader = "Rip-Worker-Draining"
)

// errWorkerMarked503 aborts a proxy attempt on a marked 503 so the retry
// loop can select another upstream. Only raised for replayable requests.
var errWorkerMarked503 = errors.New("worker answered a marked 503")

func marked503(resp *http.Response) bool {
	return resp.StatusCode == http.StatusServiceUnavailable &&
		(resp.Header.Get(workerBusyHeader) != "" || resp.Header.Get(workerDrainingHeader) != "")
}

// replayable reports whether the request can be safely delivered to another
// upstream after an attempt that received a response: with no body to
// stream (and none chunked), nothing was consumed that a retry would need.
func replayable(r *http.Request) bool {
	return r.ContentLength == 0 && len(r.TransferEncoding) == 0
}

// sockHost encodes a socket path as a synthetic URL host so one shared
// transport pools connections per socket.
func sockHost(path string) string { return hex.EncodeToString([]byte(path)) }

func (dp *dataPlane) dialUpstream(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	raw, err := hex.DecodeString(host)
	if err != nil {
		return nil, &dialError{path: host, err: fmt.Errorf("bad upstream address: %w", err)}
	}
	path := string(raw)
	d := net.Dialer{Timeout: upstreamDialTimeout}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, &dialError{path: path, err: err}
	}
	return conn, nil
}

// proxyWorkers streams the request to a healthy worker (least-conn). A failed
// dial marks that socket unhealthy and tries the next; when every socket is
// unhealthy the answer is 503 + Retry-After. 502 is reserved for a dial that
// succeeded followed by a misbehaving worker.
func (dp *dataPlane) proxyWorkers(w http.ResponseWriter, r *http.Request, rec AppRecord) error {
	tried := map[string]bool{}
	sawBusy := false
	for {
		path, ok := dp.acquireUpstream(rec.Upstreams, tried)
		if !ok {
			if sawBusy {
				// Every upstream bounced a marked 503 — capacity, not
				// failure. Answer 503 + Retry-After without the health
				// warning (and without the per-request log: under a burst
				// this is the common path).
				w.Header().Set("Retry-After", retryAfter)
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "all workers busy", http.StatusServiceUnavailable)
				return nil
			}
			return dp.unavailable(w, rec.ID, "all upstreams unhealthy")
		}
		tried[path] = true
		final, busy := dp.proxyOnce(w, r, path)
		if final {
			return nil
		}
		sawBusy = sawBusy || busy
	}
}

// acquireUpstream picks the healthy, untried worker socket with the fewest
// in-flight requests (random among ties) and charges it one in-flight.
func (dp *dataPlane) acquireUpstream(ups []Upstream, tried map[string]bool) (string, bool) {
	now := time.Now()
	dp.mu.Lock()
	defer dp.mu.Unlock()

	best := -1
	var ties []string
	for _, u := range ups {
		if u.Doorbell || tried[u.Path] {
			continue
		}
		st := dp.state[u.Path]
		if st != nil && now.Before(st.unhealthyUntil) {
			continue
		}
		inflight := 0
		if st != nil {
			inflight = st.inflight
		}
		switch {
		case best == -1 || inflight < best:
			best = inflight
			ties = ties[:0]
			ties = append(ties, u.Path)
		case inflight == best:
			ties = append(ties, u.Path)
		}
	}
	if len(ties) == 0 {
		return "", false
	}
	path := ties[rand.IntN(len(ties))]
	st := dp.state[path]
	if st == nil {
		st = &upstreamState{}
		dp.state[path] = st
	}
	st.inflight++
	return path, true
}

func (dp *dataPlane) releaseUpstream(path string) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if st := dp.state[path]; st != nil && st.inflight > 0 {
		st.inflight--
	}
}

func (dp *dataPlane) markUnhealthy(path string) {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	st := dp.state[path]
	if st == nil {
		st = &upstreamState{}
		dp.state[path] = st
	}
	st.unhealthyUntil = time.Now().Add(dp.unhealthyWindow)
}

// proxyOnce streams the request to one worker socket. It returns final=true
// when the attempt concluded the request (response written, or client gone)
// and final=false when another upstream may be tried — either the dial
// failed (body untouched) or a replayable request received a marked busy /
// draining 503 (busy=true; never a health event).
//
// The proxy is a focused net/http/httputil.ReverseProxy over a unix-dialing
// transport rather than Caddy's reverseproxy module: that module is built to
// be provisioned cold with its own upstream/health state, while Janus selects
// upstreams per request from the hot registry. ReverseProxy still gives us
// streaming in both directions, flush-on-stream, trailers, and upgrades.
func (dp *dataPlane) proxyOnce(w http.ResponseWriter, r *http.Request, path string) (final, busy bool) {
	defer dp.releaseUpstream(path)

	canReplay := replayable(r)
	var rpErr error
	rp := &httputil.ReverseProxy{
		Transport: dp.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = sockHost(path)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if !marked503(resp) {
				return nil
			}
			if canReplay {
				// Abort this attempt (routed to ErrorHandler; the proxy
				// closes the response body) so the retry loop delivers
				// the request to another upstream.
				return errWorkerMarked503
			}
			// A body was already streamed to the worker; the bounce must
			// go to the client. Strip the internal marker headers.
			resp.Header.Del(workerBusyHeader)
			resp.Header.Del(workerDrainingHeader)
			return nil
		},
		ErrorHandler: func(_ http.ResponseWriter, _ *http.Request, err error) { rpErr = err },
		ErrorLog:     zap.NewStdLog(dp.logger),
	}

	// The transport closes the outbound body even when the dial fails; hand
	// it a wrapper so the client's body (guaranteed unread on a dial error)
	// survives for a retry on another upstream. The server closes the real
	// body itself after the handler returns.
	attempt := r
	if r.Body != nil {
		ar := *r
		ar.Body = io.NopCloser(r.Body)
		attempt = &ar
	}
	rp.ServeHTTP(w, attempt)
	if rpErr == nil {
		return true, false
	}
	if errors.Is(rpErr, errWorkerMarked503) {
		// Marked 503: flow control, not failure — no health accounting.
		return false, true
	}
	if r.Context().Err() != nil {
		return true, false // client gone; nothing to write, nothing to blame
	}

	var de *dialError
	if errors.As(rpErr, &de) {
		dp.markUnhealthy(path)
		dp.logger.Warn("janus upstream dial failed",
			zap.String("upstream", path),
			zap.Error(rpErr),
		)
		return false, false
	}

	// Dial succeeded and the worker misbehaved before a response landed.
	dp.markUnhealthy(path)
	dp.logger.Warn("janus upstream failed after dial",
		zap.String("upstream", path),
		zap.Error(rpErr),
	)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "bad gateway", http.StatusBadGateway)
	return true, false
}
