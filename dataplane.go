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
	"sync/atomic"
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

// upstreamState is per-socket passive health, least-conn accounting, and the
// socket's reusable proxy. Doorbell sockets never get an entry: they are
// excluded from health. inflight and unhealthyUntil are atomics so release
// and health marking never touch dp.mu — the request path acquires the
// global mutex exactly once, in acquireUpstream.
type upstreamState struct {
	inflight       atomic.Int64
	unhealthyUntil atomic.Int64 // time.Time UnixNano; 0 = never marked
	proxy          *httputil.ReverseProxy
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

	// buffers is the ReverseProxy copy-buffer pool (32KB, matching the
	// proxy's own default size) — without it every response copy allocates.
	buffers *bufferPool

	mu      sync.Mutex
	state   map[string]*upstreamState // socket path → health + inflight + proxy
	flights map[string]*ringFlight    // app id → the one outstanding ring
}

// proxyBufSize is the ReverseProxy copy-buffer size (the proxy's own default).
const proxyBufSize = 32 << 10

// bufferPool adapts sync.Pool to httputil.BufferPool. It stores fixed-size
// array pointers, not slices: a []byte put into sync.Pool's `any` boxes the
// slice header onto the heap — one allocation per response copy.
type bufferPool struct{ p sync.Pool }

func newBufferPool() *bufferPool {
	return &bufferPool{p: sync.Pool{New: func() any { return new([proxyBufSize]byte) }}}
}

func (b *bufferPool) Get() []byte    { return b.p.Get().(*[proxyBufSize]byte)[:] }
func (b *bufferPool) Put(buf []byte) { b.p.Put((*[proxyBufSize]byte)(buf)) }

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
		buffers:         newBufferPool(),
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
	return dp.serveResolved(w, r, host, rec)
}

// serveResolved continues the decision table from an already-resolved
// record (the cache resolves once and reuses the record; the ring loop
// still re-resolves after every wake).
func (dp *dataPlane) serveResolved(w http.ResponseWriter, r *http.Request, host string, rec AppRecord) error {
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
			var ok bool
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
// The split is manual — net.SplitHostPort heap-allocates an *AddrError on
// every portless Host, which is the common case — but keeps its semantics:
// a bracketed IPv6 host loses brackets and port, an unbracketed host with
// exactly one colon loses the port, anything else passes through whole.
func normalizeHostHeader(hostport string) string {
	host := hostport
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		if host[0] == '[' {
			if j := strings.IndexByte(host, ']'); j == i-1 {
				host = host[1:j] // "[::1]:8443" → "::1"
			}
		} else if strings.IndexByte(host, ':') == i {
			host = host[:i] // "app.test:8443" → "app.test"
		}
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

// Worker-marked 503s (docs/20260719-002000-pool-protocol.md "Data plane
// decision table"): a worker at its concurrency cap answers 503 with
// Rip-Worker-Busy: 1 (drains: Rip-Worker-Draining: 1). Those are protocol
// flow control, not failures — they never count toward health, and when the
// request is replayable (no body was sent) Janus immediately tries the next
// upstream instead of forwarding the bounce to the client.
const (
	workerBusyHeader     = "Rip-Worker-Busy"
	workerDrainingHeader = "Rip-Worker-Draining"

	// ripMarkHeader is the tenant's internal correlation id header.
	ripMarkHeader = "Rip-Mark"
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
	var tried map[string]bool // allocated lazily: only a retry needs it
	sawBusy := false
	for {
		path, st, ok := dp.acquireUpstream(rec.Upstreams, tried)
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
		final, busy := dp.proxyOnce(w, r, path, st)
		if final {
			return nil
		}
		if tried == nil {
			tried = make(map[string]bool, len(rec.Upstreams))
		}
		tried[path] = true
		sawBusy = sawBusy || busy
	}
}

// acquireUpstream picks the healthy, untried worker socket with the fewest
// in-flight requests (uniform random among ties, via reservoir sampling so
// no ties slice is built under the lock), charges it one in-flight, and
// returns its state — the proxy, the lock-free release, and health marking
// all ride the returned pointer, so this is the request's only dp.mu
// acquisition.
func (dp *dataPlane) acquireUpstream(ups []Upstream, tried map[string]bool) (string, *upstreamState, bool) {
	now := time.Now().UnixNano()
	dp.mu.Lock()
	defer dp.mu.Unlock()

	var best int64
	bestIdx := -1
	ties := 0
	for i := range ups {
		u := &ups[i]
		if u.Doorbell || tried[u.Path] {
			continue
		}
		var inflight int64
		if st := dp.state[u.Path]; st != nil {
			if until := st.unhealthyUntil.Load(); until != 0 && now < until {
				continue
			}
			inflight = st.inflight.Load()
		}
		switch {
		case bestIdx == -1 || inflight < best:
			best = inflight
			bestIdx = i
			ties = 1
		case inflight == best:
			ties++
			if rand.IntN(ties) == 0 {
				bestIdx = i
			}
		}
	}
	if bestIdx == -1 {
		return "", nil, false
	}
	path := ups[bestIdx].Path
	st := dp.state[path]
	if st == nil {
		st = &upstreamState{}
		dp.state[path] = st
	}
	if st.proxy == nil {
		st.proxy = dp.newProxy(path)
	}
	st.inflight.Add(1)
	return path, st, true
}

// markUnhealthy deselects the upstream for the unhealthy window. A plain
// atomic store: concurrent markings all land inside the same window, so
// last-writer-wins matches the previous mutex-serialized behavior.
func (dp *dataPlane) markUnhealthy(st *upstreamState) {
	st.unhealthyUntil.Store(time.Now().Add(dp.unhealthyWindow).UnixNano())
}

// attemptState is one proxy attempt's per-request state, carried on the
// outbound request context so per-socket ReverseProxy structs are reusable.
type attemptState struct {
	canReplay bool
	err       error
}

type attemptKey struct{}

func attemptOf(ctx context.Context) *attemptState {
	st, _ := ctx.Value(attemptKey{}).(*attemptState)
	return st
}

// newProxy builds the reusable ReverseProxy for one socket path; it lives on
// the socket's upstreamState (built once, under acquireUpstream's lock). The
// struct carries no per-request state (that lives in attemptState), so one
// instance serves every request to that socket.
//
// The proxy is a focused net/http/httputil.ReverseProxy over a unix-dialing
// transport rather than Caddy's reverseproxy module: that module is built to
// be provisioned cold with its own upstream/health state, while Janus selects
// upstreams per request from the hot registry. ReverseProxy still gives us
// streaming in both directions, flush-on-stream, trailers, and upgrades.
func (dp *dataPlane) newProxy(path string) *httputil.ReverseProxy {
	host := sockHost(path)
	rp := &httputil.ReverseProxy{
		Transport:  dp.transport,
		BufferPool: dp.buffers,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = host
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// Scrub the internal correlation id from every client response
			// (surfacing it in the access log is future work).
			resp.Header.Del(ripMarkHeader)
			if !marked503(resp) {
				return nil
			}
			if st := attemptOf(resp.Request.Context()); st != nil && st.canReplay {
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
		ErrorHandler: func(_ http.ResponseWriter, r *http.Request, err error) {
			if st := attemptOf(r.Context()); st != nil {
				st.err = err
			}
		},
		ErrorLog: zap.NewStdLog(dp.logger),
	}
	return rp
}

// proxyOnce streams the request to one worker socket. It returns final=true
// when the attempt concluded the request (response written, or client gone)
// and final=false when another upstream may be tried — either the dial
// failed (body untouched) or a replayable request received a marked busy /
// draining 503 (busy=true; never a health event).
func (dp *dataPlane) proxyOnce(w http.ResponseWriter, r *http.Request, path string, st *upstreamState) (final, busy bool) {
	defer st.inflight.Add(-1)

	at := &attemptState{canReplay: replayable(r)}

	// The transport closes the outbound body even when the dial fails; hand
	// it a wrapper so the client's body (guaranteed unread on a dial error)
	// survives for a retry on another upstream. The server closes the real
	// body itself after the handler returns. http.NoBody needs no shield:
	// closing it is a no-op and there is nothing to replay.
	attempt := r.WithContext(context.WithValue(r.Context(), attemptKey{}, at))
	if r.Body != nil && r.Body != http.NoBody {
		attempt.Body = io.NopCloser(r.Body)
	}
	st.proxy.ServeHTTP(w, attempt)
	if at.err == nil {
		return true, false
	}
	if errors.Is(at.err, errWorkerMarked503) {
		// Marked 503: flow control, not failure — no health accounting.
		return false, true
	}
	if r.Context().Err() != nil {
		return true, false // client gone; nothing to write, nothing to blame
	}

	var de *dialError
	if errors.As(at.err, &de) {
		dp.markUnhealthy(st)
		dp.logger.Warn("janus upstream dial failed",
			zap.String("upstream", path),
			zap.Error(at.err),
		)
		return false, false
	}

	// Dial succeeded and the worker misbehaved before a response landed.
	dp.markUnhealthy(st)
	dp.logger.Warn("janus upstream failed after dial",
		zap.String("upstream", path),
		zap.Error(at.err),
	)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "bad gateway", http.StatusBadGateway)
	return true, false
}
