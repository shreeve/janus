package janus

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// --- harness ------------------------------------------------------------------

// cacheHarness wires a Handler with an effective cache-on site config over
// a real registry + data plane, with an injectable cache clock.
type cacheHarness struct {
	h     *Handler
	reg   *appRegistry
	dp    *dataPlane
	store *cacheStore
}

func newCacheHarness(t *testing.T) *cacheHarness {
	t.Helper()
	reg := newAppRegistry()
	dp := newDataPlane(reg, nil)
	store := newCacheStore(defaultCacheMaxBytes, defaultCacheAppShare)
	reg.purge = store.purgeApp
	h := &Handler{
		dp: dp,
		cacheCfg: &cacheSite{
			store:   store,
			ttl:     time.Second,
			ttlMax:  10 * time.Second,
			maxBody: defaultCacheMaxBody,
			debug:   true,
		},
	}
	return &cacheHarness{h: h, reg: reg, dp: dp, store: store}
}

// get serves one request through the cache-enabled handler. Extra headers
// come as k, v pairs.
func (ch *cacheHarness) get(t *testing.T, host, target string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	return ch.do(t, http.MethodGet, host, target, kv...)
}

func (ch *cacheHarness) do(t *testing.T, method, host, target string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	rr := httptest.NewRecorder()
	if err := ch.h.ServeHTTP(rr, r, nil); err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return rr
}

func (ch *cacheHarness) stats() cacheStats { return ch.store.snapshot() }

// flightWaiters reads the waiter count of the coalescing key's flight.
func (ch *cacheHarness) flightWaiters(host, target string, hdr http.Header) int {
	key := host + "\n" + target
	ckey := coalesceKeyFor(key, hdr)
	sh := ch.store.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if f := sh.flights[ckey]; f != nil {
		return f.waiters
	}
	return -1
}

// countingUpstream answers 200 with the given extra headers and body, and
// counts requests. A non-nil hold channel blocks every request until the
// channel closes (after the headers-received signal).
type countingUpstream struct {
	hits    atomic.Int32
	entered chan struct{} // buffered; one token per request that arrives
	hold    chan struct{}
	status  int
	header  map[string]string
	body    string
}

func (u *countingUpstream) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		if u.entered != nil {
			u.entered <- struct{}{}
		}
		if u.hold != nil {
			<-u.hold
		}
		for k, v := range u.header {
			w.Header().Set(k, v)
		}
		status := u.status
		if status == 0 {
			status = http.StatusOK
		}
		body := u.body
		if body == "" {
			body = "body:" + r.RequestURI
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(status)
		w.Write([]byte(body))
	})
}

// register starts the upstream and registers app "app" for the host.
func (ch *cacheHarness) register(t *testing.T, host string, u *countingUpstream) string {
	t.Helper()
	sock := startUnixHTTP(t, u.handler())
	return registerApp(t, ch.reg, host, Upstream{Path: sock})
}

// --- key construction -----------------------------------------------------------

func TestCacheKeyIsWireBytes(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	// /a%2Fb and /a/b are two keys: fill and admit each independently.
	for range 3 {
		ch.get(t, "app.test", "/a%2Fb")
	}
	for range 3 {
		ch.get(t, "app.test", "/a/b")
	}
	// 2 fills per key (doorkeeper admits on the second), 1 hit each.
	if got := u.hits.Load(); got != 4 {
		t.Fatalf("want 4 upstream requests for two distinct keys, got %d", got)
	}
	s := ch.stats()
	if s.Hits != 2 || s.Stores != 2 {
		t.Fatalf("want 2 hits and 2 stores, got hits=%d stores=%d", s.Hits, s.Stores)
	}
	// The hit bodies stay per-key.
	if body := ch.get(t, "app.test", "/a%2Fb").Body.String(); body != "body:/a%2Fb" {
		t.Fatalf("encoded-path key served %q", body)
	}
}

func TestCacheKeyLengthCapBypasses(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	long := "/?q=" + strings.Repeat("x", cacheKeyMax)
	for range 3 {
		ch.get(t, "app.test", long)
	}
	if got := u.hits.Load(); got != 3 {
		t.Fatalf("oversize key must bypass every time; upstream got %d", got)
	}
	if s := ch.stats(); s.Bypass != 3 || s.Stores != 0 {
		t.Fatalf("want 3 bypasses and 0 stores, got %+v", s.cacheStatsBucket)
	}
}

// --- the bypass table -------------------------------------------------------------

func TestCacheRequestBypassTable(t *testing.T) {
	cases := []struct {
		name   string
		method string
		kv     []string
		bypass bool
	}{
		{"plain GET", "GET", nil, false},
		{"POST", "POST", nil, true},
		{"HEAD", "HEAD", nil, true},
		{"Cookie", "GET", []string{"Cookie", "a=1"}, true},
		{"Authorization", "GET", []string{"Authorization", "Bearer x"}, true},
		{"Proxy-Authorization", "GET", []string{"Proxy-Authorization", "Basic x"}, true},
		{"Range", "GET", []string{"Range", "bytes=0-1"}, true},
		{"If-None-Match", "GET", []string{"If-None-Match", `"abc"`}, true},
		{"If-Modified-Since", "GET", []string{"If-Modified-Since", "Mon, 01 Jan 2024 00:00:00 GMT"}, true},
		{"If-Match", "GET", []string{"If-Match", `"abc"`}, true},
		{"If-Unmodified-Since", "GET", []string{"If-Unmodified-Since", "Mon, 01 Jan 2024 00:00:00 GMT"}, true},
		{"If-Range", "GET", []string{"If-Range", `"abc"`}, true},
		// Request Cache-Control and Pragma are ignored — not a bypass.
		{"request Cache-Control ignored", "GET", []string{"Cache-Control", "no-cache"}, false},
		{"Pragma ignored", "GET", []string{"Pragma", "no-cache"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/", nil)
			for i := 0; i+1 < len(tc.kv); i += 2 {
				r.Header.Add(tc.kv[i], tc.kv[i+1])
			}
			if got := cacheBypassRequest(r); got != tc.bypass {
				t.Fatalf("cacheBypassRequest = %v, want %v", got, tc.bypass)
			}
		})
	}
}

func TestCacheCookieBypassEndToEnd(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	for range 3 {
		rr := ch.get(t, "app.test", "/", "Cookie", "session=1")
		if got := rr.Header().Get(cacheDebugHeader); got != "BYPASS" {
			t.Fatalf("want X-Janus-Cache BYPASS, got %q", got)
		}
	}
	if got := u.hits.Load(); got != 3 {
		t.Fatalf("Cookie requests must all reach the worker, got %d", got)
	}
	if s := ch.stats(); s.Bypass != 3 || s.Stores != 0 || s.Hits != 0 {
		t.Fatalf("want bypass=3 stores=0 hits=0, got %+v", s.cacheStatsBucket)
	}
}

// --- the never-store table ---------------------------------------------------------

func TestCacheNeverStoreTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header map[string]string
		stored bool
	}{
		{"plain 200", 200, nil, true},
		{"404", 404, nil, false},
		{"500", 500, nil, false},
		{"unmarked 503", 503, nil, false},
		{"Set-Cookie", 200, map[string]string{"Set-Cookie": "sid=1"}, false},
		{"no-store", 200, map[string]string{"Cache-Control": "no-store"}, false},
		{"no-cache", 200, map[string]string{"Cache-Control": "no-cache"}, false},
		{"private", 200, map[string]string{"Cache-Control": "private"}, false},
		{"unparseable Cache-Control", 200, map[string]string{"Cache-Control": "max-age=="}, false},
		{"Expires presence vetoes", 200, map[string]string{"Expires": "Fri, 01 Jan 2100 00:00:00 GMT"}, false},
		{"Age vetoes", 200, map[string]string{"Age": "3"}, false},
		{"Content-Encoding without Vary", 200, map[string]string{"Content-Encoding": "gzip"}, false},
		{"Content-Encoding identity ok", 200, map[string]string{"Content-Encoding": "identity"}, true},
		{"Content-Encoding with Vary Accept-Encoding", 200,
			map[string]string{"Content-Encoding": "gzip", "Vary": "Accept-Encoding"}, true},
		{"ACAO echo", 200, map[string]string{"Access-Control-Allow-Origin": "https://evil.test"}, false},
		{"ACAO star ok", 200, map[string]string{"Access-Control-Allow-Origin": "*"}, true},
		{"max-age=0", 200, map[string]string{"Cache-Control": "max-age=0"}, false},
		{"Vary star", 200, map[string]string{"Vary": "*"}, false},
		{"Vary Cookie", 200, map[string]string{"Vary": "Cookie"}, false},
		{"Vary User-Agent", 200, map[string]string{"Vary": "User-Agent"}, false},
		{"Vary allowlisted", 200, map[string]string{"Vary": "Accept-Language"}, true},
		{"max-age positive", 200, map[string]string{"Cache-Control": "max-age=5"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := newCacheHarness(t)
			u := &countingUpstream{status: tc.status, header: tc.header, body: "payload"}
			ch.register(t, "app.test", u)

			// Two requests: the doorkeeper admits on the second fill.
			// The client declares Accept-Encoding so the proxy transport
			// never transparently decompresses (which would strip the
			// Content-Encoding the table is testing).
			ch.get(t, "app.test", "/", "Accept-Encoding", "gzip")
			ch.get(t, "app.test", "/", "Accept-Encoding", "gzip")
			s := ch.stats()
			wantStores := int64(0)
			if tc.stored {
				wantStores = 1
			}
			if s.Stores != wantStores {
				t.Fatalf("stores = %d, want %d (%+v)", s.Stores, wantStores, s.cacheStatsBucket)
			}
			if !tc.stored && u.hits.Load() != 2 {
				t.Fatalf("non-storable responses must all reach the worker, got %d", u.hits.Load())
			}
		})
	}
}

func TestCacheTrailersNeverStored(t *testing.T) {
	ch := newCacheHarness(t)
	// A chunked response with real trailers: the proxy announces them in
	// the Trailer header, which the never-store table rejects.
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Checksum")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("chunked body"))
		w.Header().Set("X-Checksum", "abc")
	}))
	registerApp(t, ch.reg, "app.test", Upstream{Path: sock})

	ch.get(t, "app.test", "/")
	ch.get(t, "app.test", "/")
	if s := ch.stats(); s.Stores != 0 {
		t.Fatalf("trailer-bearing response stored: %+v", s.cacheStatsBucket)
	}
}

func TestCacheTruncatedFillNeverStored(t *testing.T) {
	ch := newCacheHarness(t)
	// Declares Content-Length 1000 but sends 500 bytes: Go's server slams
	// the connection, the proxy aborts (ErrAbortHandler), and the fill
	// must resolve without storing.
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 500))
	}))
	registerApp(t, ch.reg, "app.test", Upstream{Path: sock})

	serve := func() {
		defer func() { recover() }() // the abort panic is the server's business
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
	}
	serve()
	serve()
	if s := ch.stats(); s.Stores != 0 {
		t.Fatalf("truncated fill stored: %+v", s.cacheStatsBucket)
	}
}

// --- HIT behavior --------------------------------------------------------------

func TestCacheHitServesWithoutWorker(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{body: "hello"}
	ch.register(t, "app.test", u)

	r1 := ch.get(t, "app.test", "/page")
	r2 := ch.get(t, "app.test", "/page")
	r3 := ch.get(t, "app.test", "/page")
	if got := u.hits.Load(); got != 2 {
		t.Fatalf("want 2 worker requests (doorkeeper admits on the second), got %d", got)
	}
	if r3.Body.String() != r1.Body.String() || r3.Body.String() != "hello" {
		t.Fatalf("hit body %q differs from fill %q", r3.Body.String(), r1.Body.String())
	}
	if r3.Header().Get("Age") == "" {
		t.Fatal("hit missing Age header")
	}
	if got := r3.Header().Get(cacheDebugHeader); got != "HIT" {
		t.Fatalf("want X-Janus-Cache HIT, got %q", got)
	}
	if got := r2.Header().Get(cacheDebugHeader); got != "MISS" {
		t.Fatalf("want X-Janus-Cache MISS on the fill, got %q", got)
	}
	s := ch.stats()
	if s.Hits != 1 || s.Misses != 2 || s.AdmissionRejects != 1 || s.Stores != 1 {
		t.Fatalf("want hits=1 misses=2 admission_rejects=1 stores=1, got %+v", s.cacheStatsBucket)
	}
	if a := s.Apps; len(a) == 0 {
		t.Fatal("per-app breakdown missing")
	}
}

func TestCacheDebugHeaderOffByDefault(t *testing.T) {
	ch := newCacheHarness(t)
	ch.h.cacheCfg.debug = false
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	for range 3 {
		rr := ch.get(t, "app.test", "/")
		if got := rr.Header().Get(cacheDebugHeader); got != "" {
			t.Fatalf("debug off must emit no %s, got %q", cacheDebugHeader, got)
		}
	}
	if s := ch.stats(); s.Hits != 1 {
		t.Fatalf("want 1 hit, got %+v", s.cacheStatsBucket)
	}
}

func TestCacheTTLExpiry(t *testing.T) {
	ch := newCacheHarness(t)
	now := time.Now()
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	ch.store.now = func() time.Time { return time.Unix(0, clock.Load()) }
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	ch.get(t, "app.test", "/") // fill (admission reject)
	ch.get(t, "app.test", "/") // fill (stored, ttl 1s)
	ch.get(t, "app.test", "/") // hit
	if got := u.hits.Load(); got != 2 {
		t.Fatalf("want 2 fills before expiry, got %d", got)
	}
	clock.Store(now.Add(1200 * time.Millisecond).UnixNano())
	ch.get(t, "app.test", "/") // expired → miss → fill
	if got := u.hits.Load(); got != 3 {
		t.Fatalf("expired entry must refill; upstream got %d", got)
	}
}

func TestCacheTTLAnchorIsFillStart(t *testing.T) {
	ch := newCacheHarness(t)
	base := time.Now()
	var clock atomic.Int64
	clock.Store(base.UnixNano())
	ch.store.now = func() time.Time { return time.Unix(0, clock.Load()) }

	// Each request holds until it receives one token.
	hold := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: hold}
	ch.register(t, "app.test", u)

	serve := func() chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = "app.test"
			ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
		}()
		return done
	}

	// First fill primes the doorkeeper (not admitted).
	d1 := serve()
	<-u.entered
	hold <- struct{}{}
	<-d1

	// Second fill starts at base and completes 900ms later: the stored
	// entry's age must anchor at fill START.
	d2 := serve()
	<-u.entered
	clock.Store(base.Add(900 * time.Millisecond).UnixNano())
	hold <- struct{}{}
	<-d2
	if s := ch.stats(); s.Stores != 1 {
		t.Fatalf("precondition: want 1 store, got %+v", s.cacheStatsBucket)
	}

	// 1.1s after fill start (only 200ms after the store): expired. A
	// fill-end anchor would still serve it.
	clock.Store(base.Add(1100 * time.Millisecond).UnixNano())
	d3 := serve()
	<-u.entered // reaches the worker again: the entry was expired
	hold <- struct{}{}
	<-d3
	if got := u.hits.Load(); got != 3 {
		t.Fatalf("entry outlived ttl measured from fill start (hits %d)", got)
	}
}

// --- Vary ---------------------------------------------------------------------

func TestCacheVaryVariantsCoexist(t *testing.T) {
	ch := newCacheHarness(t)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := r.Header.Get("Accept-Language")
		w.Header().Set("Vary", "Accept-Language")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("lang:" + lang))
	}))
	registerApp(t, ch.reg, "app.test", Upstream{Path: sock})

	de := []string{"Accept-Language", "de"}
	en := []string{"Accept-Language", "en"}
	ch.get(t, "app.test", "/", de...)
	ch.get(t, "app.test", "/", de...) // de stored
	ch.get(t, "app.test", "/", en...)
	ch.get(t, "app.test", "/", en...) // en stored
	r5 := ch.get(t, "app.test", "/", de...)
	if r5.Body.String() != "lang:de" {
		t.Fatalf("de variant lost: %q", r5.Body.String())
	}
	if got := r5.Header().Get(cacheDebugHeader); got != "HIT" {
		t.Fatalf("fifth request want HIT, got %q", got)
	}
	s := ch.stats()
	if s.Stores != 2 || s.Entries != 2 {
		t.Fatalf("both variants must coexist under one primary key: %+v", s.cacheStatsBucket)
	}
	// And the en variant still hits too.
	if r6 := ch.get(t, "app.test", "/", en...); r6.Body.String() != "lang:en" {
		t.Fatalf("en variant lost: %q", r6.Body.String())
	}
}

func TestParseVary(t *testing.T) {
	cases := []struct {
		in    []string
		names []string
		ok    bool
	}{
		{nil, nil, true},
		{[]string{"Accept-Encoding"}, []string{"accept-encoding"}, true},
		{[]string{"accept-encoding"}, []string{"accept-encoding"}, true}, // h2 lowercase
		{[]string{" Accept ,\tAccept-Language "}, []string{"accept", "accept-language"}, true},
		{[]string{"Accept", "Accept-Language"}, []string{"accept", "accept-language"}, true}, // multi-line
		{[]string{"Accept, Accept"}, []string{"accept"}, true},                               // dup collapses
		{[]string{"*"}, nil, false},
		{[]string{"Cookie"}, nil, false},
		{[]string{"User-Agent"}, nil, false},
		{[]string{"Accept, X-Custom"}, nil, false}, // one bad member fails closed
		{[]string{""}, nil, false},                 // empty member is not recognized
	}
	for _, tc := range cases {
		h := http.Header{}
		for _, v := range tc.in {
			h.Add("Vary", v)
		}
		names, ok := parseVary(h)
		if ok != tc.ok {
			t.Fatalf("parseVary(%q) ok=%v, want %v", tc.in, ok, tc.ok)
		}
		if tc.ok && strings.Join(names, ",") != strings.Join(tc.names, ",") {
			t.Fatalf("parseVary(%q) = %v, want %v", tc.in, names, tc.names)
		}
	}
}

func TestHeaderValueKeyAbsentVsEmpty(t *testing.T) {
	absent := http.Header{}
	empty := http.Header{"Accept": {""}}
	multi := http.Header{"Accept": {"a", "b"}}
	if headerValueKey(absent, "Accept") == headerValueKey(empty, "Accept") {
		t.Fatal("absent header must be distinct from an empty one")
	}
	if got := headerValueKey(multi, "Accept"); !strings.HasSuffix(got, "a, b") {
		t.Fatalf("multi-line join = %q, want arrival-order \", \" join", got)
	}
}

// --- TTL arithmetic -------------------------------------------------------------

func TestFreshnessRules(t *testing.T) {
	siteTTL, ttlMax := time.Second, 10*time.Second
	cases := []struct {
		cc   string
		want time.Duration
	}{
		{"", time.Second},                          // default: site ttl
		{"max-age=5", 5 * time.Second},             // max-age
		{"s-maxage=3, max-age=7", 3 * time.Second}, // s-maxage wins
		{"max-age=99", 10 * time.Second},           // capped at ttl_max
		{"s-maxage=99", 10 * time.Second},          // capped at ttl_max
		{"max-age=0", 0},                           // do not reuse
	}
	for _, tc := range cases {
		var vals []string
		if tc.cc != "" {
			vals = []string{tc.cc}
		}
		cc, ok := parseCacheControl(vals)
		if !ok {
			t.Fatalf("parseCacheControl(%q) failed", tc.cc)
		}
		if got := freshnessFor(cc, siteTTL, ttlMax); got != tc.want {
			t.Fatalf("freshness(%q) = %v, want %v", tc.cc, got, tc.want)
		}
	}
}

func TestParseCacheControl(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{"no-store", true},
		{"private, max-age=60", true},
		{`private="set-cookie", max-age=60`, true},
		{"max-age=abc", false},
		{"max-age=", false},
		{"max-age==", false},
		{`no-store, "`, false},
		{"=5", false},
	}
	for _, tc := range cases {
		if _, ok := parseCacheControl([]string{tc.in}); ok != tc.ok {
			t.Fatalf("parseCacheControl(%q) ok=%v, want %v", tc.in, ok, tc.ok)
		}
	}
}

// --- doorkeeper -----------------------------------------------------------------

func TestDoorkeeperAdmitsOnSecondSighting(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{}
	ch.register(t, "app.test", u)

	ch.get(t, "app.test", "/one-hit")
	s := ch.stats()
	if s.Stores != 0 || s.AdmissionRejects != 1 {
		t.Fatalf("first fill must be doorkeeper-rejected: %+v", s.cacheStatsBucket)
	}
	ch.get(t, "app.test", "/one-hit")
	if s := ch.stats(); s.Stores != 1 {
		t.Fatalf("second fill must store: %+v", s.cacheStatsBucket)
	}
}

// --- max_body -------------------------------------------------------------------

func TestCacheMaxBodyStreamsUncached(t *testing.T) {
	ch := newCacheHarness(t)
	ch.h.cacheCfg.maxBody = 8
	u := &countingUpstream{body: "0123456789abcdef"} // 16 bytes > cap
	ch.register(t, "app.test", u)

	for range 3 {
		rr := ch.get(t, "app.test", "/")
		if rr.Body.String() != "0123456789abcdef" {
			t.Fatalf("oversize response mangled: %q", rr.Body.String())
		}
	}
	if got := u.hits.Load(); got != 3 {
		t.Fatalf("oversize bodies must never be served from cache, got %d upstream hits", got)
	}
	if s := ch.stats(); s.Stores != 0 {
		t.Fatalf("oversize body stored: %+v", s.cacheStatsBucket)
	}
}

// --- coalescing -----------------------------------------------------------------

func TestCoalescingNMissesOneFill(t *testing.T) {
	ch := newCacheHarness(t)
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 64), hold: release, body: "shared"}
	ch.register(t, "app.test", u)

	const n = 8
	var wg sync.WaitGroup
	results := make([]*httptest.ResponseRecorder, n)
	// Leader first, deterministically.
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		results[0] = rr
	}()
	<-u.entered // the leader is inside the worker

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = "app.test"
			rr := httptest.NewRecorder()
			ch.h.ServeHTTP(rr, r, nil)
			results[i] = rr
		}()
	}
	waitFor(t, "waiters joined", func() bool {
		return ch.flightWaiters("app.test", "/", http.Header{}) == n-1
	})
	close(release)
	wg.Wait()

	if got := u.hits.Load(); got != 1 {
		t.Fatalf("want exactly 1 origin request for %d concurrent misses, got %d", n, got)
	}
	for i, rr := range results {
		if rr.Code != http.StatusOK || rr.Body.String() != "shared" {
			t.Fatalf("request %d: got %d %q", i, rr.Code, rr.Body.String())
		}
	}
	s := ch.stats()
	if s.Coalesced != n-1 || s.Misses != 1 {
		t.Fatalf("want coalesced=%d misses=1, got %+v", n-1, s.cacheStatsBucket)
	}
	// COALESCED responses carry no Age (they are the fill, not a reuse).
	for i := 1; i < n; i++ {
		if v := results[i].Header().Get(cacheDebugHeader); v != "COALESCED" && v != "MISS" {
			t.Fatalf("waiter %d verdict %q", i, v)
		}
	}
}

func TestCoalescingWaiterCapOverflowFallsThrough(t *testing.T) {
	ch := newCacheHarness(t)
	ch.store.waiterCap = 2
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: release, body: "x"}
	ch.register(t, "app.test", u)

	var wg sync.WaitGroup
	serve := func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		if rr.Code != http.StatusOK {
			t.Errorf("nobody gets a manufactured 503; got %d", rr.Code)
		}
	}
	wg.Add(1)
	go serve()
	<-u.entered // leader in the worker
	for range 2 {
		wg.Add(1)
		go serve()
	}
	waitFor(t, "two waiters", func() bool {
		return ch.flightWaiters("app.test", "/", http.Header{}) == 2
	})
	wg.Add(1)
	go serve() // third waiter overflows the cap → falls through to the worker
	<-u.entered
	close(release)
	wg.Wait()

	if got := u.hits.Load(); got != 2 {
		t.Fatalf("want leader + 1 overflow fall-through at the worker, got %d", got)
	}
	if s := ch.stats(); s.WaiterOverflow != 1 {
		t.Fatalf("want waiter_overflow=1, got %+v", s.cacheStatsBucket)
	}
}

func TestCoalescingWaiterDeadlineFallsThrough(t *testing.T) {
	ch := newCacheHarness(t)
	ch.store.waiterDeadline = 30 * time.Millisecond
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: release, body: "x"}
	ch.register(t, "app.test", u)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // leader, held by the slow worker
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
	}()
	<-u.entered

	wg.Add(1)
	var waiter *httptest.ResponseRecorder
	go func() { // waiter whose deadline expires mid-hold
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		waiter = rr
	}()
	<-u.entered // the expired waiter fell through and reached the worker
	close(release)
	wg.Wait()

	if waiter.Code != http.StatusOK {
		t.Fatalf("expired waiter must fall through, not 503: %d", waiter.Code)
	}
	if s := ch.stats(); s.WaiterExpired != 1 {
		t.Fatalf("want waiter_expired=1, got %+v", s.cacheStatsBucket)
	}
}

func TestCoalescingFillFailureFallsThroughAndMarks(t *testing.T) {
	ch := newCacheHarness(t)
	release := make(chan struct{})
	u := &countingUpstream{
		entered: make(chan struct{}, 16),
		hold:    release,
		header:  map[string]string{"Set-Cookie": "sid=1"},
		body:    "personal",
	}
	ch.register(t, "app.test", u)

	var wg sync.WaitGroup
	var leader, waiter *httptest.ResponseRecorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		leader = rr
	}()
	<-u.entered
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		waiter = rr
	}()
	waitFor(t, "one waiter", func() bool {
		return ch.flightWaiters("app.test", "/", http.Header{}) == 1
	})
	close(release)
	wg.Wait()

	// The leader's per-client response was never shared: the waiter fell
	// through and got its own worker response.
	if got := u.hits.Load(); got != 2 {
		t.Fatalf("want 2 worker requests (leader + fall-through), got %d", got)
	}
	if leader.Header().Get("Set-Cookie") == "" || waiter.Header().Get("Set-Cookie") == "" {
		t.Fatal("each client must receive its own Set-Cookie response")
	}
	if s := ch.stats(); s.Coalesced != 0 || s.Stores != 0 {
		t.Fatalf("nothing shared, nothing stored: %+v", s.cacheStatsBucket)
	}

	// The key now carries a do-not-coalesce mark: the next request
	// bypasses without buffering for one ttl.
	rr := ch.get(t, "app.test", "/")
	if got := rr.Header().Get(cacheDebugHeader); got != "BYPASS" {
		t.Fatalf("marked key must bypass, got %q", got)
	}
	if s := ch.stats(); s.Bypass != 1 {
		t.Fatalf("want bypass=1 after the mark, got %+v", s.cacheStatsBucket)
	}
}

// --- generation fence ------------------------------------------------------------

func TestGenerationFenceRejectsStraddlingFill(t *testing.T) {
	ch := newCacheHarness(t)
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: release, body: "old-gen"}
	sockOld := startUnixHTTP(t, u.handler())
	appID := registerApp(t, ch.reg, "app.test", Upstream{Path: sockOld})

	// The fill that will straddle the cut (fence checks before doorkeeper,
	// so fenced_stores is the counter that must move).
	prime := make(chan struct{})
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
		close(prime)
	}()
	<-u.entered

	// Mid-fill: PUT new upstreams — the purge event bumps the generation.
	uNew := &countingUpstream{body: "new-gen"}
	sockNew := startUnixHTTP(t, uNew.handler())
	if _, err := ch.reg.setUpstreams(appID, []Upstream{{Path: sockNew}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-prime

	s := ch.stats()
	if s.FencedStores != 1 {
		t.Fatalf("straddling fill must be fence-rejected: %+v", s.cacheStatsBucket)
	}
	if s.Stores != 0 || s.Entries != 0 {
		t.Fatalf("nothing may be stored across the cut: %+v", s.cacheStatsBucket)
	}
	// The next request misses and fills from the NEW pool.
	rr := ch.get(t, "app.test", "/")
	if rr.Body.String() != "new-gen" {
		t.Fatalf("post-cut request served %q, want new-gen", rr.Body.String())
	}
	if uNew.hits.Load() != 1 {
		t.Fatalf("post-cut request must reach the new worker, got %d", uNew.hits.Load())
	}
}

func TestPurgeDetachesWaiters(t *testing.T) {
	ch := newCacheHarness(t)
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: release, body: "old"}
	sockOld := startUnixHTTP(t, u.handler())
	appID := registerApp(t, ch.reg, "app.test", Upstream{Path: sockOld})

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
	}()
	<-u.entered

	var waiter *httptest.ResponseRecorder
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		rr := httptest.NewRecorder()
		ch.h.ServeHTTP(rr, r, nil)
		waiter = rr
	}()
	waitFor(t, "one waiter", func() bool {
		return ch.flightWaiters("app.test", "/", http.Header{}) == 1
	})

	// The purge detaches the flight: the waiter falls through to the data
	// plane and reaches the NEW worker while the old fill still hangs.
	uNew := &countingUpstream{body: "new"}
	sockNew := startUnixHTTP(t, uNew.handler())
	if _, err := ch.reg.setUpstreams(appID, []Upstream{{Path: sockNew}}); err != nil {
		t.Fatal(err)
	}
	<-waiterDone
	if waiter.Code != http.StatusOK || waiter.Body.String() != "new" {
		t.Fatalf("detached waiter got %d %q, want the new worker's response", waiter.Code, waiter.Body.String())
	}
	close(release)
	<-leaderDone
	if s := ch.stats(); s.FencedStores != 1 {
		t.Fatalf("the straddling leader's store must be fenced: %+v", s.cacheStatsBucket)
	}
}

func TestStaleFlightNeverJoined(t *testing.T) {
	ch := newCacheHarness(t)
	release := make(chan struct{})
	u := &countingUpstream{entered: make(chan struct{}, 16), hold: release, body: "x"}
	appID := ch.register(t, "app.test", u)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
	}()
	<-u.entered

	// Bump the generation WITHOUT the purge walk (simulating the race
	// window): a new arrival must not join the stale flight — it falls
	// through and reaches the worker itself.
	ch.reg.mu.Lock()
	ch.reg.apps[appID].gen.Add(1)
	ch.reg.mu.Unlock()

	arrival := make(chan struct{})
	go func() {
		defer close(arrival)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "app.test"
		ch.h.ServeHTTP(httptest.NewRecorder(), r, nil)
	}()
	<-u.entered // the arrival is at the worker, not waiting on the flight
	if got := u.hits.Load(); got != 2 {
		t.Fatalf("stale-flight arrival must reach the worker itself, got %d", got)
	}
	close(release)
	<-arrival
	<-done
}

func TestHostReclaimNeverServesOldTenant(t *testing.T) {
	ch := newCacheHarness(t)
	// Simulate the O5 race: entries survive a delete because the purge
	// hook is disconnected (as if the drop lost a race). The HIT-path
	// app-id validation must still refuse the old tenant's bytes.
	uA := &countingUpstream{body: "tenant-A"}
	sockA := startUnixHTTP(t, uA.handler())
	recA, err := ch.reg.create("appa", []string{"claim.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.reg.setUpstreams(recA.ID, []Upstream{{Path: sockA}}); err != nil {
		t.Fatal(err)
	}
	ch.get(t, "claim.test", "/")
	ch.get(t, "claim.test", "/") // stored for app A
	if s := ch.stats(); s.Stores != 1 {
		t.Fatalf("precondition: want 1 store, got %+v", s.cacheStatsBucket)
	}

	ch.reg.purge = nil // the simulated lost purge
	if err := ch.reg.delete(recA.ID); err != nil {
		t.Fatal(err)
	}
	uB := &countingUpstream{body: "tenant-B"}
	sockB := startUnixHTTP(t, uB.handler())
	recB, err := ch.reg.create("appb", []string{"claim.test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ch.reg.setUpstreams(recB.ID, []Upstream{{Path: sockB}}); err != nil {
		t.Fatal(err)
	}

	rr := ch.get(t, "claim.test", "/")
	if rr.Body.String() != "tenant-B" {
		t.Fatalf("re-claimed host served the previous tenant's bytes: %q", rr.Body.String())
	}
	if uB.hits.Load() != 1 {
		t.Fatalf("request must reach tenant B's worker, got %d", uB.hits.Load())
	}
}

func TestPurgeOnUpstreamsPutEmptiesKeys(t *testing.T) {
	ch := newCacheHarness(t)
	u := &countingUpstream{body: "v1"}
	appID := ch.register(t, "app.test", u)

	ch.get(t, "app.test", "/")
	ch.get(t, "app.test", "/") // stored
	if s := ch.stats(); s.Entries != 1 {
		t.Fatalf("precondition: want 1 entry, got %+v", s.cacheStatsBucket)
	}
	u2 := &countingUpstream{body: "v2"}
	sock2 := startUnixHTTP(t, u2.handler())
	if _, err := ch.reg.setUpstreams(appID, []Upstream{{Path: sock2}}); err != nil {
		t.Fatal(err)
	}
	s := ch.stats()
	if s.Entries != 0 || s.Purges == 0 {
		t.Fatalf("PUT upstreams must purge: %+v", s.cacheStatsBucket)
	}
	// The immediate next request reaches the NEW worker.
	rr := ch.get(t, "app.test", "/")
	if rr.Body.String() != "v2" || u2.hits.Load() != 1 {
		t.Fatalf("post-purge request served %q (new worker hits %d)", rr.Body.String(), u2.hits.Load())
	}
}

// --- memory bounds ---------------------------------------------------------------

// primeAndStore inserts an entry directly through storeFill with the
// doorkeeper pre-bumped and a matching generation.
func primeAndStore(c *cacheStore, appID, key string, gen *atomic.Uint64, body []byte, access time.Time) bool {
	sh := c.shardFor(key)
	ck := coalesceKeyFor(key, http.Header{})
	hash := c.hash(ck)
	sh.mu.Lock()
	sh.dkBump(hash)
	sh.dkBump(hash)
	sh.mu.Unlock()
	f := &cacheFlight{appID: appID, gen: gen, genSnap: gen.Load()}
	before := sh.ctr.stores.Load()
	c.storeFill(sh, key, f, hash, nil, nil, http.StatusOK, http.Header{}, body, access, time.Hour)
	return sh.ctr.stores.Load() == before+1
}

func TestCacheByteAccountingAndEviction(t *testing.T) {
	// One shard's budget is maxBytes/cacheShardCount; overflow evicts LRU.
	c := newCacheStore(cacheShardCount*4096, 100)
	gen := new(atomic.Uint64)
	base := time.Now()
	c.now = func() time.Time { return base }

	key := "h\n/k0"
	sh := c.shardFor(key)
	if !primeAndStore(c, "app", key, gen, make([]byte, 1024), base) {
		t.Fatal("first store rejected")
	}
	wantSize := int64(1024) + int64(len(key)) + cacheEntryOverhead
	if got := sh.bytes; got != wantSize {
		t.Fatalf("accounted bytes = %d, want %d", got, wantSize)
	}
	// Fill the shard past its 4096 budget; evictions must keep it under.
	for i := 1; i < 8; i++ {
		k := fmt.Sprintf("h\n/k%d-%d", i, c.hash(key)) // same shard not guaranteed; use distinct keys anyway
		primeAndStore(c, "app", k, gen, make([]byte, 1024), base.Add(time.Duration(i)*time.Millisecond))
	}
	for _, s := range c.shards {
		if s.bytes > s.budget {
			t.Fatalf("shard over budget: %d > %d", s.bytes, s.budget)
		}
	}
	snap := c.snapshot()
	if snap.StoredBytes <= 0 || snap.Entries <= 0 {
		t.Fatalf("gauges broken: %+v", snap.cacheStatsBucket)
	}
}

func TestCacheAppShareCap(t *testing.T) {
	// Ample per-shard budget (8kb per shard) but a 1% app share (~1.7
	// entries): the share cap, not the shard budget, must bind. Keys are
	// picked to land in one shard so the within-app eviction is
	// observable.
	c := newCacheStore(cacheShardCount*8192, 1)
	gen := new(atomic.Uint64)
	base := time.Now()
	c.now = func() time.Time { return base }

	target := c.shardFor("h\n/s0")
	keys := []string{"h\n/s0"}
	for i := 1; len(keys) < 6; i++ {
		k := fmt.Sprintf("h\n/s%d", i)
		if c.shardFor(k) == target {
			keys = append(keys, k)
		}
	}
	for i, k := range keys {
		primeAndStore(c, "hog", k, gen, make([]byte, 1024), base.Add(time.Duration(i)*time.Millisecond))
	}
	if got := c.appBytes("hog"); got > c.appShareBytes {
		t.Fatalf("app exceeded its share: %d > %d", got, c.appShareBytes)
	}
	if snap := c.snapshot(); snap.Evictions == 0 {
		t.Fatalf("share pressure must evict within the app: %+v", snap.cacheStatsBucket)
	}
	if snap := c.snapshot(); snap.Stores == 0 {
		t.Fatalf("share cap must not block every store: %+v", snap.cacheStatsBucket)
	}
}

// --- config: parse + cascade -------------------------------------------------------

func TestParseCacheDirectiveGlobal(t *testing.T) {
	d := caddyfile.NewTestDispenser(`janus {
		cache {
			ttl 2s
			ttl_max 20s
			max_body 1kb
			max_bytes 32mb
			max_app_share 25
			debug
		}
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	cs := app.Cache
	if cs == nil || cs.Enabled == nil || !*cs.Enabled {
		t.Fatal("cache with a block must be enabled")
	}
	if time.Duration(*cs.TTL) != 2*time.Second || time.Duration(*cs.TTLMax) != 20*time.Second {
		t.Fatalf("ttl parse: %v / %v", *cs.TTL, *cs.TTLMax)
	}
	if *cs.MaxBody != 1000 || *cs.MaxBytes != 32_000_000 || *cs.MaxAppShare != 25 || !*cs.Debug {
		t.Fatalf("knob parse: %d %d %d %v", *cs.MaxBody, *cs.MaxBytes, *cs.MaxAppShare, *cs.Debug)
	}
}

func TestParseCacheDirectiveHardErrors(t *testing.T) {
	globalCases := []string{
		`janus { cache maybe }`,
		`janus { cache on off }`,
		`janus { cache off { ttl 1s } }`,
		`janus { cache { bogus 1 } }`,
		`janus { cache { ttl } }`,
		`janus { cache { ttl abc } }`,
		`janus { cache { ttl 0s } }`,
		`janus { cache { ttl -1s } }`,
		`janus { cache { max_body 0 } }`,
		`janus { cache { max_body nope } }`,
		`janus { cache { max_bytes 0 } }`,
		`janus { cache { max_app_share 0 } }`,
		`janus { cache { max_app_share 101 } }`,
		`janus { cache { max_app_share half } }`,
		`janus { cache { debug loudly } }`,
		`janus { cache { ttl 1s { nested } } }`,
		`janus { cache { ttl 1s ttl 2s } }`,
		`janus { cache { } cache on }`,
	}
	for _, cf := range globalCases {
		d := caddyfile.NewTestDispenser(cf)
		if err := new(App).UnmarshalCaddyfile(d); err == nil {
			t.Errorf("global parse accepted %q", cf)
		}
	}
	siteCases := []string{
		`janus { cache { max_bytes 1mb } }`,
		`janus { cache { max_app_share 10 } }`,
		`janus { cache off { debug } }`,
		`janus { cache on cache off }`,
	}
	for _, cf := range siteCases {
		d := caddyfile.NewTestDispenser(cf)
		var h Handler
		if err := h.UnmarshalCaddyfile(d); err == nil {
			t.Errorf("site parse accepted %q", cf)
		}
	}
}

func caddyDurPtr(d time.Duration) *caddy.Duration {
	cd := caddy.Duration(d)
	return &cd
}

func TestCacheCascade(t *testing.T) {
	onV, offV := true, false
	ttlG := caddyDurPtr(time.Second)
	ttlS := caddyDurPtr(5 * time.Second)
	cases := []struct {
		name         string
		site, global *CacheSettings
		wantOn       bool
		wantTTL      time.Duration
	}{
		{"unset/unset → off", nil, nil, false, 0},
		{"global on", nil, &CacheSettings{Enabled: &onV}, true, time.Second},
		{"global on, site off", &CacheSettings{Enabled: &offV}, &CacheSettings{Enabled: &onV}, false, 0},
		{"global off, site on", &CacheSettings{Enabled: &onV}, &CacheSettings{Enabled: &offV}, true, time.Second},
		{"site overrides one key", &CacheSettings{Enabled: &onV, TTL: ttlS},
			&CacheSettings{Enabled: &onV, TTL: ttlG}, true, 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &App{Cache: tc.global}
			app.appsReg = newAppRegistry()
			if err := app.provisionCacheStore(); err != nil {
				t.Fatal(err)
			}
			app.dp = newDataPlane(app.appsReg, nil)
			h := &Handler{Cache: tc.site, app: app, dp: app.dp}
			if err := h.provisionCache(); err != nil {
				t.Fatal(err)
			}
			if on := h.cacheCfg != nil; on != tc.wantOn {
				t.Fatalf("effective on = %v, want %v", on, tc.wantOn)
			}
			if tc.wantOn && h.cacheCfg.ttl != tc.wantTTL {
				t.Fatalf("effective ttl = %v, want %v", h.cacheCfg.ttl, tc.wantTTL)
			}
		})
	}
}

func TestCacheProvisionTTLMaxOrdering(t *testing.T) {
	on := true
	bad := &CacheSettings{Enabled: &on, TTL: caddyDurPtr(5 * time.Second), TTLMax: caddyDurPtr(time.Second)}
	app := &App{}
	app.appsReg = newAppRegistry()
	if err := app.provisionCacheStore(); err != nil {
		t.Fatal(err)
	}
	app.dp = newDataPlane(app.appsReg, nil)
	h := &Handler{Cache: bad, app: app, dp: app.dp}
	if err := h.provisionCache(); err == nil {
		t.Fatal("ttl_max < ttl must fail provision")
	}
	appBad := &App{Cache: bad}
	appBad.appsReg = newAppRegistry()
	if err := appBad.provisionCacheStore(); err == nil {
		t.Fatal("global ttl_max < ttl must fail provision")
	}
}

func TestCacheSiteRejectsProcessWideKeysAtProvision(t *testing.T) {
	on := true
	mb := int64(1 << 20)
	h := &Handler{Cache: &CacheSettings{Enabled: &on, MaxBytes: &mb}}
	if err := h.provisionCache(); err == nil {
		t.Fatal("site max_bytes must fail provision (JSON path)")
	}
}
