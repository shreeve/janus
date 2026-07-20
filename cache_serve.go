package janus

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// The cache request path (docs/20260720-033201-capability-microcache.md
// "The decision, per request"). Checked after ping and after host
// resolution — the cache never sees unknown hosts. Every path the rules
// cannot prove safe bypasses: bypass is exactly today's behavior.

// cacheSite is one site's effective cache configuration, resolved at
// provision (cascade: site override → global default → built-in default).
// A site whose effective cache is off has no cacheSite at all — the
// non-opted-in hot path is one nil check.
type cacheSite struct {
	store   *cacheStore
	ttl     time.Duration
	ttlMax  time.Duration
	maxBody int64
	debug   bool
}

// serveCache runs the per-request decision table for a cache-on site.
func (h *Handler) serveCache(w http.ResponseWriter, r *http.Request) error {
	cc := h.cacheCfg
	c := cc.store
	dp := h.dp

	host := normalizeHostHeader(r.Host)
	rec, ok := dp.registry.resolveHost(host) // generation snapshot rides rec.genSnap
	if !ok {
		return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("janus: unknown host %q", host))
	}

	if cacheBypassRequest(r) {
		return c.bypassServe(w, r, dp, cc, host, rec)
	}
	// Primary key: normalized host + the request-target bytes as received
	// on the wire (never a decoded or re-encoded form — /a%2Fb and /a/b
	// are two keys).
	key := host + "\n" + r.RequestURI
	if len(key) > cacheKeyMax {
		return c.bypassServe(w, r, dp, cc, host, rec)
	}
	ckey := coalesceKeyFor(key, r.Header)
	ckeyHash := c.hash(ckey)
	sh := c.shardFor(key)
	now := c.now()

	// The HIT path takes at most this one shard lock.
	sh.mu.Lock()
	sh.dkBump(ckeyHash)

	if exp, marked := sh.marks[ckey]; marked {
		if now.UnixNano() < exp {
			// Live do-not-coalesce mark: this key can't win — no
			// coalescing, no buffering, for one ttl.
			sh.mu.Unlock()
			return c.bypassServe(w, r, dp, cc, host, rec)
		}
		delete(sh.marks, ckey)
	}

	if ke := sh.keys[key]; ke != nil {
		if ke.appID != rec.ID {
			// A re-claimed host never serves the previous tenant's
			// bytes: MISS and evict.
			sh.dropKeyLocked(key, ke)
		} else {
			for i := 0; i < len(ke.variants); {
				if ke.variants[i].expired(now) {
					sh.dropVariantLocked(ke.variants[i])
					continue
				}
				i++
			}
			for _, v := range ke.variants {
				if v.variantMatch(r.Header) {
					v.lastAccess.Store(now.UnixNano())
					sh.ctr.hits.Add(1)
					v.stats.hits.Add(1)
					age := int64(now.Sub(v.fillStart) / time.Second)
					status, header, body := v.status, v.header, v.body
					sh.mu.Unlock()
					return writeCachedResponse(w, status, header, body, age, "HIT", cc.debug)
				}
			}
		}
	}

	sh.sweepLocked(now)

	if f := sh.flights[ckey]; f != nil {
		if f.genSnap != f.gen.Load() || f.detached.Load() {
			// Never join a flight whose generation is no longer current.
			sh.mu.Unlock()
			return c.bypassServe(w, r, dp, cc, host, rec)
		}
		if f.waiters >= c.waiterCap {
			sh.ctr.waiterOverflow.Add(1)
			sh.appStats(rec.ID).waiterOverflow.Add(1)
			sh.mu.Unlock()
			// Overflow falls through to the data plane — never a
			// manufactured 503; the data plane sheds at capacity anyway.
			if cc.debug {
				w.Header().Set(cacheDebugHeader, "BYPASS")
			}
			return dp.serveResolved(w, r, host, rec)
		}
		f.waiters++
		sh.mu.Unlock()
		return c.awaitFill(w, r, dp, cc, sh, f, rec)
	}

	// MISS — become the fill. The flight carries the generation snapshot
	// taken with host resolution.
	f := &cacheFlight{
		appID:   rec.ID,
		gen:     rec.gen,
		genSnap: rec.genSnap,
		done:    make(chan struct{}),
		detach:  make(chan struct{}),
	}
	sh.flights[ckey] = f
	sh.ctr.misses.Add(1)
	sh.appStats(rec.ID).misses.Add(1)
	sh.mu.Unlock()

	return c.fill(w, r, dp, cc, host, rec, sh, key, ckey, ckeyHash, f)
}

// bypassServe proceeds through the standard decision table exactly as
// today — doorbell, marked-503 retry, health accounting, all unchanged.
func (c *cacheStore) bypassServe(w http.ResponseWriter, r *http.Request, dp *dataPlane, cc *cacheSite, host string, rec AppRecord) error {
	home := c.shardFor(rec.ID)
	home.ctr.bypass.Add(1)
	home.appStats(rec.ID).bypass.Add(1)
	if cc.debug {
		w.Header().Set(cacheDebugHeader, "BYPASS")
	}
	return dp.serveResolved(w, r, host, rec)
}

func (v *cacheVariant) variantMatch(h http.Header) bool {
	for i, name := range v.varyNames {
		if headerValueKey(h, name) != v.reqVals[i] {
			return false
		}
	}
	return true
}

// writeCachedResponse serves a stored (or coalesced) response. Stored
// headers are post-scrub and hop-by-hop-free; the buffered length is the
// Content-Length. age < 0 omits the Age header (COALESCED responses are
// the fill itself, not a reused stored response).
func writeCachedResponse(w http.ResponseWriter, status int, stored http.Header, body []byte, age int64, verdict string, debug bool) error {
	hdr := w.Header()
	for k, vv := range stored {
		hdr[k] = vv // stored slices are immutable after store
	}
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	if age >= 0 {
		hdr.Set("Age", strconv.FormatInt(age, 10))
	}
	if debug {
		// Stamped at serve time, never stored.
		hdr.Set(cacheDebugHeader, verdict)
	}
	w.WriteHeader(status)
	_, err := w.Write(body)
	return err
}

// awaitFill blocks a waiter on the in-flight fill: served on a shareable
// outcome, fallen through to the data plane on anything else (fill
// failure, purge detach, deadline), abandoned when its client goes away.
func (c *cacheStore) awaitFill(w http.ResponseWriter, r *http.Request, dp *dataPlane, cc *cacheSite, sh *cacheShard, f *cacheFlight, rec AppRecord) error {
	timer := time.NewTimer(c.waiterDeadline)
	defer timer.Stop()

	select {
	case <-f.done:
		if f.shareable && !f.detached.Load() {
			sh.ctr.coalesced.Add(1)
			sh.appStats(rec.ID).coalesced.Add(1)
			return writeCachedResponse(w, f.status, f.header, f.body, -1, "COALESCED", cc.debug)
		}
		// The fill produced something per-client (or failed): the
		// leader's response goes to the leader alone; this waiter falls
		// through to the normal data plane individually.
	case <-f.detach:
		// A purge detached the flight; fall through — the data plane
		// correctly finds the doorbell and coalesces onto one ring.
	case <-timer.C:
		sh.mu.Lock()
		f.waiters--
		sh.mu.Unlock()
		sh.ctr.waiterExpired.Add(1)
		sh.appStats(rec.ID).waiterExpired.Add(1)
	case <-r.Context().Done():
		// A waiter whose client disconnects is abandoned; the fill
		// continues for the rest.
		sh.mu.Lock()
		f.waiters--
		sh.mu.Unlock()
		return nil
	}
	if cc.debug {
		w.Header().Set(cacheDebugHeader, "BYPASS")
	}
	// Fresh resolve: the registry may have changed while we held.
	return dp.serve(w, r)
}

// --- the fill (leader) ---------------------------------------------------------

// fillState is the leader-side bookkeeping for one fill. All methods run
// on the leader's goroutine.
type fillState struct {
	c         *cacheStore
	cc        *cacheSite
	sh        *cacheShard
	key       string
	ckey      string
	ckeyHash  uint64
	f         *cacheFlight
	fillStart time.Time
	released  bool
}

// release resolves the flight exactly once: removes it from the shard (a
// purge may have detached it first), optionally sets the one-ttl
// do-not-coalesce mark, publishes the shareable outcome, and wakes the
// waiters.
func (ld *fillState) release(shareable bool, status int, header http.Header, body []byte, mark bool) {
	if ld.released {
		return
	}
	ld.released = true
	sh := ld.sh
	sh.mu.Lock()
	if sh.flights[ld.ckey] == ld.f {
		delete(sh.flights, ld.ckey)
	}
	if mark {
		sh.marks[ld.ckey] = ld.c.now().Add(ld.cc.ttl).UnixNano()
	}
	sh.mu.Unlock()
	ld.f.shareable = shareable
	ld.f.status, ld.f.header, ld.f.body = status, header, body
	close(ld.f.done)
}

// fill proxies the leader's request through the normal data plane behind a
// recording writer, then decides storability and resolves the flight.
func (c *cacheStore) fill(w http.ResponseWriter, r *http.Request, dp *dataPlane, cc *cacheSite, host string, rec AppRecord, sh *cacheShard, key, ckey string, ckeyHash uint64, f *cacheFlight) error {
	// TTL anchor: fill start — before the upstream request is sent.
	ld := &fillState{c: c, cc: cc, sh: sh, key: key, ckey: ckey, ckeyHash: ckeyHash, f: f, fillStart: c.now()}
	fr := &fillRecorder{w: w, cc: cc, onUnshareable: func() {
		// Header-time early release: waiters fall through instead of
		// holding for a body that will never be shared.
		ld.release(false, 0, nil, nil, true)
	}}
	if cc.debug {
		w.Header().Set(cacheDebugHeader, "MISS")
	}

	completed := false
	defer func() {
		if !completed {
			// The proxy aborts a torn body by panicking with
			// ErrAbortHandler; the flight must not strand its waiters.
			// A truncated fill is never stored; the panic keeps
			// unwinding after this.
			ld.release(false, 0, nil, nil, true)
		}
	}()
	err := dp.serveResolved(fr, r, host, rec)
	completed = true
	ld.complete(r, fr, err)
	return err
}

// complete applies the never-store table's body-time rows and either
// shares + stores the fill or releases the waiters to fall through.
func (ld *fillState) complete(r *http.Request, fr *fillRecorder, err error) {
	storable := err == nil && fr.wroteHeader && fr.headerStorable && !fr.abandoned && !fr.writeErr
	if storable && fr.declaredCL >= 0 && fr.written != fr.declaredCL {
		storable = false // truncation / length mismatch never poisons the key
	}
	if !storable {
		// Client-gone with nothing written is not the origin's fault;
		// everything else marks the key do-not-coalesce for one ttl so a
		// never-storable hot route cannot cycle waiter pulses forever.
		mark := !(r.Context().Err() != nil && !fr.wroteHeader)
		ld.release(false, 0, nil, nil, mark)
		return
	}

	header := cleanStoredHeader(fr.header)
	body := append([]byte(nil), fr.buf...) // trim to exact length before store
	sort.Strings(fr.varyNames)
	reqVals := make([]string, len(fr.varyNames))
	for i, name := range fr.varyNames {
		reqVals[i] = headerValueKey(r.Header, name)
	}

	// Sharing is gated by storability — the proof the response is not
	// per-client — never by whether the bytes happen to be retained.
	ld.release(true, fr.status, header, body, false)
	ld.c.storeFill(ld.sh, ld.key, ld.f, ld.ckeyHash, fr.varyNames, reqVals, fr.status, header, body, ld.fillStart, fr.freshness)
}

// storeFill admits a storable fill into the shard: generation fence,
// doorkeeper, per-app share cap, shard budget — in that order.
func (c *cacheStore) storeFill(sh *cacheShard, key string, f *cacheFlight, ckeyHash uint64, varyNames, reqVals []string, status int, header http.Header, body []byte, fillStart time.Time, fresh time.Duration) {
	appID := f.appID
	size := int64(len(body)) + int64(len(key)) + headerBytes(header) + cacheEntryOverhead
	if size > sh.budget {
		return // cannot fit even after eviction; simply not stored
	}
	now := c.now()

	sh.mu.Lock()
	defer sh.mu.Unlock()

	// The fence, inside the shard lock: a racing purge either bumped the
	// generation before this compare (store rejected) or walks the shards
	// after it (entry dropped). Either way no pre-cut bytes survive the
	// cut.
	if f.gen.Load() != f.genSnap {
		sh.ctr.fencedStores.Add(1)
		sh.appStats(appID).fencedStores.Add(1)
		return
	}
	// Doorkeeper: only a key seen at least twice this window is admitted;
	// one-hit wonders never enter the LRU.
	if sh.dkCount(ckeyHash) < 2 {
		sh.ctr.admissionRejects.Add(1)
		sh.appStats(appID).admissionRejects.Add(1)
		return
	}
	// Per-app share cap: evict within the app first.
	for c.appShareBytes > 0 && c.appBytes(appID)+size > c.appShareBytes {
		if !sh.evictLocked(now, appID) {
			return // the app's share is spent in other shards; not stored
		}
	}
	// Shard budget: LRU by bytes.
	for sh.bytes+size > sh.budget {
		if !sh.evictLocked(now, "") {
			return
		}
	}

	v := &cacheVariant{
		key:       key,
		appID:     appID,
		varyNames: varyNames,
		reqVals:   reqVals,
		status:    status,
		header:    header,
		body:      body,
		fillStart: fillStart,
		fresh:     fresh,
		size:      size,
		stats:     sh.appStats(appID),
	}
	v.lastAccess.Store(now.UnixNano())
	sh.insertVariantLocked(v)
	sh.ctr.stores.Add(1)
	v.stats.stores.Add(1)
}

// --- the recording writer --------------------------------------------------------

// hopByHopHeaders are never stored; a HIT serves the buffered length as
// Content-Length instead.
var hopByHopHeaders = []string{
	"Transfer-Encoding", "Connection", "Keep-Alive", "TE", "Trailer",
	"Upgrade", "Proxy-Connection", "Content-Length",
}

func cleanStoredHeader(h http.Header) http.Header {
	out := h.Clone()
	for _, k := range hopByHopHeaders {
		out.Del(k)
	}
	out.Del(cacheDebugHeader) // stamped at serve time, never stored
	return out
}

func headerBytes(h http.Header) int64 {
	var n int64
	for k, vv := range h {
		for _, v := range vv {
			n += int64(len(k) + len(v))
		}
	}
	return n
}

// fillRecorder tees the fill's response to the client while buffering a
// storable copy up to max_body. Header-time storability is decided at
// WriteHeader; a non-storable response releases the waiters immediately
// and stops buffering (the client's stream is untouched either way).
type fillRecorder struct {
	w  http.ResponseWriter
	cc *cacheSite

	onUnshareable func() // fired once, at most, on the leader's goroutine

	wroteHeader    bool
	status         int
	header         http.Header // final-header clone (post-scrub bytes)
	headerStorable bool
	varyNames      []string
	freshness      time.Duration
	declaredCL     int64 // -1 = none declared
	buf            []byte
	abandoned      bool
	writeErr       bool
	written        int64
}

func (fr *fillRecorder) Header() http.Header { return fr.w.Header() }

func (fr *fillRecorder) WriteHeader(code int) {
	if code >= 100 && code < 200 {
		// Interim responses go to the leader's connection only; they are
		// never stored.
		fr.w.WriteHeader(code)
		return
	}
	if !fr.wroteHeader {
		fr.wroteHeader = true
		fr.status = code
		fr.header = fr.w.Header().Clone()
		fr.evaluateHeaders()
		if !fr.headerStorable {
			fr.abandoned = true
			fr.buf = nil
			fr.onUnshareable()
		}
	}
	fr.w.WriteHeader(code)
}

func (fr *fillRecorder) Write(b []byte) (int, error) {
	if !fr.wroteHeader {
		fr.WriteHeader(http.StatusOK)
	}
	if !fr.abandoned {
		if int64(len(fr.buf))+int64(len(b)) > fr.cc.maxBody {
			// Oversize discovered mid-body: abandon — buffered bytes
			// already flushed to the client, the rest streams untouched.
			fr.buf = nil
			fr.abandoned = true
			fr.onUnshareable()
		} else {
			fr.buf = append(fr.buf, b...)
		}
	}
	n, err := fr.w.Write(b)
	fr.written += int64(n)
	if err != nil {
		fr.writeErr = true
	}
	return n, err
}

func (fr *fillRecorder) Unwrap() http.ResponseWriter { return fr.w }

func (fr *fillRecorder) Flush() {
	if f, ok := fr.w.(http.Flusher); ok {
		f.Flush()
	}
}

// evaluateHeaders applies every never-store row decidable from the
// response headers alone (docs/20260720-033201-capability-microcache.md
// "Never store"). Anything the rules cannot prove safe is not storable.
func (fr *fillRecorder) evaluateHeaders() {
	fr.declaredCL = -1
	h := fr.header
	if cl := h.Get("Content-Length"); cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 {
			return
		}
		fr.declaredCL = n
	}
	if fr.status != http.StatusOK {
		return // 200 only — subsumes marked 503s and every non-200
	}
	if len(h["Set-Cookie"]) > 0 {
		return // per-client by definition; non-negotiable
	}
	cc, ok := parseCacheControl(h["Cache-Control"])
	if !ok || cc.noStore || cc.noCache || cc.private {
		return // origin veto, or a veto the cache failed to read
	}
	if len(h["Expires"]) > 0 {
		return // presence is the veto; the date is never parsed
	}
	if len(h["Age"]) > 0 {
		return // already-consumed freshness this cache would re-grant
	}
	varyNames, ok := parseVary(h)
	if !ok {
		return // Vary outside the allowlist fails closed
	}
	if ce := h.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		// Encoding variance without a matching Vary is never acceptable:
		// the failure is not wrong content, it is garbage bytes.
		hasAE := false
		for _, n := range varyNames {
			if n == "accept-encoding" {
				hasAE = true
				break
			}
		}
		if !hasAE {
			return
		}
	}
	if acao := h["Access-Control-Allow-Origin"]; len(acao) > 0 &&
		(len(acao) != 1 || acao[0] != "*") {
		return // anything but a static * is echoing or conditional
	}
	if len(h["Trailer"]) > 0 || len(h["Upgrade"]) > 0 {
		return // streaming/tunnel semantics don't buffer
	}
	fresh := freshnessFor(cc, fr.cc.ttl, fr.cc.ttlMax)
	if fresh <= 0 {
		return // max-age=0 means "do not reuse"
	}
	if fr.declaredCL > fr.cc.maxBody {
		return // oversize by Content-Length up front
	}
	fr.varyNames = varyNames
	fr.freshness = fresh
	fr.headerStorable = true
}
