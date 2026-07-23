package janus

import (
	"hash/maphash"
	"net/http"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Micro-cache + request coalescing store
// (docs/20260720-033201-capability-microcache.md "Correctness spec",
// "Memory bounds", "Concurrency structure").
//
// The store is sharded by primary-key hash. Each shard owns its mutex, its
// byte budget (max_bytes / shards), its per-app key index for purge, its
// doorkeeper, its do-not-coalesce marks, and its in-flight coalescing
// flights. The HIT path takes at most one shard lock; recency is an atomic
// last-access timestamp with sampled eviction, so a HIT does no list splice.
// Counters are per-shard padded atomics summed at read time; the /1.0/cache
// snapshot never blocks stores or hits.

const (
	// cacheShardCount is the shard fan-out (spec: 16–64 by key hash).
	cacheShardCount = 32

	// cacheKeyMax caps the primary key; longer keys bypass — a minted key
	// can never be megabyte-scale.
	cacheKeyMax = 8 << 10

	// cacheWaiterCap bounds waiters per coalescing key (same shape and
	// magnitude as the ring's defaultWaiterCap). Overflow falls through
	// to the data plane — never a manufactured 503.
	cacheWaiterCap = 64

	// cacheWaiterDeadline is the wall-clock cap on one waiter's hold
	// (ring-timeout order). The shipped data plane has no proxy response
	// timeout, so this deadline is defined here — there is nothing to
	// inherit. Expiry → that waiter falls through individually.
	cacheWaiterDeadline = 15 * time.Second

	// cacheEntryOverhead is the fixed per-entry accounting charge
	// (map buckets, entry struct, stored Vary values).
	cacheEntryOverhead = 512

	// Doorkeeper: a store is admitted only for a coalescing key seen at
	// least twice within the current window. Slots are per shard; the
	// window resets after resetAfter bumps.
	doorkeeperSlots      = 8192 // power of two
	doorkeeperResetAfter = doorkeeperSlots * 8

	// cacheDebugHeader is stamped at serve time (never stored) when the
	// site's cache debug knob is on.
	cacheDebugHeader = "X-Janus-Cache"
)

// Built-in defaults (spec "Defaults").
const (
	defaultCacheTTL      = time.Second
	defaultCacheTTLMax   = 10 * time.Second
	defaultCacheMaxBody  = 256 << 10
	defaultCacheMaxBytes = 64 << 20
	defaultCacheAppShare = 50 // percent of max_bytes one app may hold
)

// varyAllowlist is the fixed set of request headers a response Vary may
// name and still be stored (lowercase; not configurable in v1). Anything
// else — Vary: *, Vary: Cookie, … — is never stored.
var varyAllowlist = [3]string{"accept", "accept-encoding", "accept-language"}

// cacheCounters is one padded block of counters — used per shard for the
// process totals and per app (within shards) for the /1.0/cache breakdown.
type cacheCounters struct {
	hits             atomic.Int64
	misses           atomic.Int64
	coalesced        atomic.Int64
	bypass           atomic.Int64
	stores           atomic.Int64
	purges           atomic.Int64
	evictions        atomic.Int64
	fencedStores     atomic.Int64
	admissionRejects atomic.Int64
	waiterOverflow   atomic.Int64
	waiterExpired    atomic.Int64
	entries          atomic.Int64 // gauge
	storedBytes      atomic.Int64 // gauge
	_                [24]byte     // pad 13×8 → 128 (cache-line multiple)
}

// cacheVariant is one stored response under a primary key: the entry
// remembers the response Vary names and the request-header values it was
// filled under; a lookup matches only when the current request's values
// for those names are identical.
type cacheVariant struct {
	key       string   // primary key (for index removal)
	appID     string   // validated on HIT against the resolved record
	varyNames []string // lowercase allowlisted names from the response Vary
	reqVals   []string // shared-extraction values, parallel to varyNames

	status    int // always 200
	header    http.Header
	body      []byte
	fillStart time.Time     // TTL anchor: when the fill's upstream request was sent
	fresh     time.Duration // effective freshness
	size      int64         // accounted bytes (body + headers + key + overhead)

	lastAccess atomic.Int64   // UnixNano; sampled-LRU recency
	stats      *cacheCounters // this shard's per-app counters
	idx        int            // position in shard.all (sampled eviction)
}

func (v *cacheVariant) expired(now time.Time) bool {
	return now.Sub(v.fillStart) >= v.fresh
}

// cacheKeyEntry is the set of variants stored under one primary key.
type cacheKeyEntry struct {
	appID    string
	variants []*cacheVariant
}

// cacheFlight is one in-progress fill; waiters on the same coalescing key
// await done. A purge detaches the flight: waiters are released to fall
// through to the data plane individually, and new arrivals never join.
type cacheFlight struct {
	appID   string
	gen     *atomic.Uint64 // the app's generation counter (shared pointer)
	genSnap uint64         // snapshot taken with host resolution

	done   chan struct{} // closed by the leader when the fill resolves
	detach chan struct{} // closed by a purge; waiters fall through

	waiters  int         // guarded by the shard mutex
	detached atomic.Bool // set before detach closes

	// Result, written before done closes. Shared with waiters only when
	// shareable (storable 200, generation current, not detached).
	shareable bool
	status    int
	header    http.Header
	body      []byte
}

// cacheShard is one lock domain of the store.
type cacheShard struct {
	mu      sync.Mutex
	keys    map[string]*cacheKeyEntry      // primary key → variants
	all     []*cacheVariant                // sampled-LRU eviction pool
	byApp   map[string]map[string]struct{} // app id → primary keys (purge index)
	marks   map[string]int64               // coalescing key → do-not-coalesce expiry (UnixNano)
	flights map[string]*cacheFlight        // coalescing key → in-flight fill

	dk        [doorkeeperSlots]uint8
	dkSamples int

	bytes  int64 // accounted bytes (guarded by mu)
	budget int64

	ctr  cacheCounters
	apps sync.Map // app id → *cacheCounters (lock-free snapshot)
}

func (sh *cacheShard) appStats(id string) *cacheCounters {
	if v, ok := sh.apps.Load(id); ok {
		return v.(*cacheCounters)
	}
	v, _ := sh.apps.LoadOrStore(id, &cacheCounters{})
	return v.(*cacheCounters)
}

// --- doorkeeper --------------------------------------------------------------

// dkBump records one lookup of the coalescing-key hash (two saturating
// counting-Bloom positions). The window resets periodically so one-hit
// wonders never accumulate admission. Caller holds sh.mu.
func (sh *cacheShard) dkBump(h uint64) {
	sh.dkSamples++
	if sh.dkSamples >= doorkeeperResetAfter {
		sh.dkSamples = 0
		clear(sh.dk[:])
	}
	i1 := h & (doorkeeperSlots - 1)
	i2 := (h >> 32) & (doorkeeperSlots - 1)
	if sh.dk[i1] < 255 {
		sh.dk[i1]++
	}
	if sh.dk[i2] < 255 {
		sh.dk[i2]++
	}
}

// dkCount reports the (conservative) times the hash was seen this window.
// Caller holds sh.mu.
func (sh *cacheShard) dkCount(h uint64) uint8 {
	i1 := h & (doorkeeperSlots - 1)
	i2 := (h >> 32) & (doorkeeperSlots - 1)
	return min(sh.dk[i1], sh.dk[i2])
}

// --- store -------------------------------------------------------------------

// cacheStore is the process-wide pool (one per Janus process; always
// constructed so /1.0/cache counters are always on).
type cacheStore struct {
	seed   maphash.Seed
	shards [cacheShardCount]*cacheShard

	maxBytes      int64
	appShareBytes int64 // max_app_share percent of maxBytes

	waiterCap      int
	waiterDeadline time.Duration

	// now is the cache clock; tests inject a fake.
	now func() time.Time
}

func newCacheStore(maxBytes int64, appSharePct int) *cacheStore {
	c := &cacheStore{
		seed:           maphash.MakeSeed(),
		maxBytes:       maxBytes,
		appShareBytes:  maxBytes / 100 * int64(appSharePct),
		waiterCap:      cacheWaiterCap,
		waiterDeadline: cacheWaiterDeadline,
		now:            time.Now,
	}
	budget := maxBytes / cacheShardCount
	for i := range c.shards {
		c.shards[i] = &cacheShard{
			keys:    map[string]*cacheKeyEntry{},
			byApp:   map[string]map[string]struct{}{},
			marks:   map[string]int64{},
			flights: map[string]*cacheFlight{},
			budget:  budget,
		}
	}
	return c
}

func (c *cacheStore) hash(s string) uint64 { return maphash.String(c.seed, s) }

func (c *cacheStore) shardFor(key string) *cacheShard {
	return c.shards[c.hash(key)%cacheShardCount]
}

// appBytes sums the app's accounted bytes across shards (store-time share
// check; lock-free sync.Map loads, no shard mutexes).
func (c *cacheStore) appBytes(appID string) int64 {
	var total int64
	for _, sh := range c.shards {
		if v, ok := sh.apps.Load(appID); ok {
			total += v.(*cacheCounters).storedBytes.Load()
		}
	}
	return total
}

// purgeApp drops every entry stored for the app and detaches its in-flight
// coalescing flights (waiters fall through to the data plane individually).
// The generation bump happened in the registry's critical section before
// this walk, so a fill that straddles the purge can neither store (fence)
// nor retain waiters (detach + join-reject).
func (c *cacheStore) purgeApp(appID string) {
	for _, sh := range c.shards {
		sh.mu.Lock()
		if keys := sh.byApp[appID]; keys != nil {
			for key := range keys {
				if ke := sh.keys[key]; ke != nil {
					sh.dropKeyLocked(key, ke)
				}
			}
			delete(sh.byApp, appID)
		}
		for ckey, f := range sh.flights {
			if f.appID != appID {
				continue
			}
			delete(sh.flights, ckey)
			f.detached.Store(true)
			close(f.detach)
		}
		sh.mu.Unlock()
	}
	home := c.shardFor(appID)
	home.ctr.purges.Add(1)
	home.appStats(appID).purges.Add(1)
}

// --- shard-locked entry bookkeeping -------------------------------------------

// insertVariantLocked adds v to the shard maps and gauges. Caller holds sh.mu.
func (sh *cacheShard) insertVariantLocked(v *cacheVariant) {
	ke := sh.keys[v.key]
	if ke == nil || ke.appID != v.appID {
		if ke != nil {
			sh.dropKeyLocked(v.key, ke) // stale tenant under the key
		}
		ke = &cacheKeyEntry{appID: v.appID}
		sh.keys[v.key] = ke
	}
	// Replace an exact-duplicate variant (same Vary names and values).
	for i, old := range ke.variants {
		if variantSelectorEqual(old, v) {
			sh.unaccountLocked(old)
			sh.allRemoveLocked(old)
			ke.variants[i] = v
			sh.allAddLocked(v)
			sh.accountLocked(v)
			return
		}
	}
	ke.variants = append(ke.variants, v)
	keys := sh.byApp[v.appID]
	if keys == nil {
		keys = map[string]struct{}{}
		sh.byApp[v.appID] = keys
	}
	keys[v.key] = struct{}{}
	sh.allAddLocked(v)
	sh.accountLocked(v)
}

func variantSelectorEqual(a, b *cacheVariant) bool {
	if len(a.varyNames) != len(b.varyNames) {
		return false
	}
	for i := range a.varyNames {
		if a.varyNames[i] != b.varyNames[i] || a.reqVals[i] != b.reqVals[i] {
			return false
		}
	}
	return true
}

func (sh *cacheShard) accountLocked(v *cacheVariant) {
	sh.bytes += v.size
	sh.ctr.entries.Add(1)
	sh.ctr.storedBytes.Add(v.size)
	v.stats.entries.Add(1)
	v.stats.storedBytes.Add(v.size)
}

func (sh *cacheShard) unaccountLocked(v *cacheVariant) {
	sh.bytes -= v.size
	sh.ctr.entries.Add(-1)
	sh.ctr.storedBytes.Add(-v.size)
	v.stats.entries.Add(-1)
	v.stats.storedBytes.Add(-v.size)
}

func (sh *cacheShard) allAddLocked(v *cacheVariant) {
	v.idx = len(sh.all)
	sh.all = append(sh.all, v)
}

func (sh *cacheShard) allRemoveLocked(v *cacheVariant) {
	last := len(sh.all) - 1
	sh.all[v.idx] = sh.all[last]
	sh.all[v.idx].idx = v.idx
	sh.all = sh.all[:last]
}

// dropVariantLocked removes one variant entirely. Caller holds sh.mu.
func (sh *cacheShard) dropVariantLocked(v *cacheVariant) {
	ke := sh.keys[v.key]
	if ke != nil {
		for i, cand := range ke.variants {
			if cand == v {
				ke.variants = append(ke.variants[:i], ke.variants[i+1:]...)
				break
			}
		}
		if len(ke.variants) == 0 {
			delete(sh.keys, v.key)
			if keys := sh.byApp[ke.appID]; keys != nil {
				delete(keys, v.key)
				if len(keys) == 0 {
					delete(sh.byApp, ke.appID)
				}
			}
		}
	}
	sh.allRemoveLocked(v)
	sh.unaccountLocked(v)
}

// dropKeyLocked removes a primary key and all its variants. Caller holds sh.mu.
func (sh *cacheShard) dropKeyLocked(key string, ke *cacheKeyEntry) {
	for _, v := range ke.variants {
		sh.allRemoveLocked(v)
		sh.unaccountLocked(v)
	}
	delete(sh.keys, key)
	if keys := sh.byApp[ke.appID]; keys != nil {
		delete(keys, key)
		if len(keys) == 0 {
			delete(sh.byApp, ke.appID)
		}
	}
}

// evictLocked frees space via sampled LRU (Redis-style: sample a few
// entries, evict the stalest; expired entries are immediate victims).
// When appOnly is non-empty, only that app's entries are candidates (the
// max_app_share path evicts within the app first). Returns false when no
// candidate remains. Caller holds sh.mu.
func (sh *cacheShard) evictLocked(now time.Time, appOnly string) bool {
	const sample = 5
	var victim *cacheVariant
	n := len(sh.all)
	if n == 0 {
		return false
	}
	// Deterministic probe walk seeded by length; sampling needs spread,
	// not cryptographic randomness.
	start := int(now.UnixNano()) % n
	if start < 0 {
		start += n
	}
	seen := 0
	for i := 0; i < n && seen < sample; i++ {
		v := sh.all[(start+i*7)%n]
		if appOnly != "" && v.appID != appOnly {
			continue
		}
		seen++
		if v.expired(now) {
			victim = v
			break
		}
		if victim == nil || v.lastAccess.Load() < victim.lastAccess.Load() {
			victim = v
		}
	}
	if victim == nil && appOnly != "" {
		// Sparse sampling missed; scan for any entry of the app.
		for _, v := range sh.all {
			if v.appID == appOnly {
				if victim == nil || v.lastAccess.Load() < victim.lastAccess.Load() {
					victim = v
				}
			}
		}
	}
	if victim == nil {
		return false
	}
	sh.dropVariantLocked(victim)
	sh.ctr.evictions.Add(1)
	victim.stats.evictions.Add(1)
	return true
}

// sweepLocked opportunistically drops a few expired entries and a few
// expired do-not-coalesce marks, so neither can accumulate unbounded
// between purges. Caller holds sh.mu.
func (sh *cacheShard) sweepLocked(now time.Time) {
	const probes = 8
	if n := len(sh.all); n > 0 {
		start := int(now.UnixNano()) % n
		if start < 0 {
			start += n
		}
		for i := 0; i < probes && len(sh.all) > 0; i++ {
			idx := (start + i*11) % len(sh.all)
			if v := sh.all[idx]; v.expired(now) {
				sh.dropVariantLocked(v)
			}
		}
	}
	if len(sh.marks) > 0 {
		nowNS := now.UnixNano()
		probed := 0
		for k, exp := range sh.marks { // map order is quasi-random
			if nowNS >= exp {
				delete(sh.marks, k)
			}
			if probed++; probed >= 4 {
				break
			}
		}
	}
}

// --- header semantics ----------------------------------------------------------

// headerValueKey is THE shared value-extraction function used by both the
// variant key and the coalescing key: multiple header lines join with ", "
// in arrival order; an absent header is its own value, distinct from an
// empty one.
func headerValueKey(h http.Header, name string) string {
	vals := h[textproto.CanonicalMIMEHeaderKey(name)]
	if len(vals) == 0 {
		return "\x00"
	}
	return "\x01" + strings.Join(vals, ", ")
}

// coalesceKeyFor is the coalescing key: the primary key plus the request's
// values of all three allowlisted headers. Finer than or equal to every
// legal storage variant key, so no waiter can receive a wrong variant.
func coalesceKeyFor(key string, h http.Header) string {
	return key + "\n" +
		headerValueKey(h, "Accept") + "\n" +
		headerValueKey(h, "Accept-Encoding") + "\n" +
		headerValueKey(h, "Accept-Language")
}

// cacheBypassRequest applies the request-side bypass table: never serve
// from cache, never store, never coalesce — exactly today's behavior.
func cacheBypassRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return true
	}
	// Authenticated traffic bypasses on the wall's EXPLICIT context
	// signal, never on Cookie-header residue: the wall strips its own
	// cookies before the request proceeds, and every response behind it
	// is per-identity by assumption.
	if authIdentityOf(r.Context()) != "" {
		return true
	}
	h := r.Header
	if len(h["Cookie"]) > 0 || len(h["Authorization"]) > 0 || len(h["Proxy-Authorization"]) > 0 {
		return true
	}
	if len(h["Range"]) > 0 {
		return true
	}
	if len(h["If-None-Match"]) > 0 || len(h["If-Modified-Since"]) > 0 ||
		len(h["If-Match"]) > 0 || len(h["If-Unmodified-Since"]) > 0 || len(h["If-Range"]) > 0 {
		return true
	}
	return false
}

// parseVary is fail-closed, mechanically: names compare case-insensitively
// (HTTP/2 delivers lowercase), values are OWS-trimmed and comma-split,
// multiple Vary lines combine, and any member not in the allowlist —
// including an empty member — means never store.
func parseVary(h http.Header) (names []string, ok bool) {
	vals := h["Vary"]
	if len(vals) == 0 {
		return nil, true
	}
	for _, line := range vals {
		for _, m := range strings.Split(line, ",") {
			m = strings.ToLower(strings.Trim(m, " \t"))
			allowed := false
			for _, a := range varyAllowlist {
				if m == a {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, false
			}
			dup := false
			for _, n := range names {
				if n == m {
					dup = true
					break
				}
			}
			if !dup {
				names = append(names, m)
			}
		}
	}
	return names, true
}

// respCacheControl is the parsed response Cache-Control. sMaxage / maxAge
// are -1 when absent.
type respCacheControl struct {
	noStore bool
	noCache bool
	private bool
	sMaxage int64
	maxAge  int64
}

func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// parseCacheControl parses the response Cache-Control lines. ok=false means
// the header did not parse — a veto the cache failed to read — never store.
func parseCacheControl(vals []string) (cc respCacheControl, ok bool) {
	cc.sMaxage, cc.maxAge = -1, -1
	s := strings.Join(vals, ",")
	i := 0
	for i < len(s) {
		// Skip OWS and empty list members.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Directive name (token).
		start := i
		for i < len(s) && isTokenChar(s[i]) {
			i++
		}
		if i == start {
			return cc, false // not a token where one is required
		}
		name := strings.ToLower(s[start:i])
		var value string
		hasValue := false
		if i < len(s) && s[i] == '=' {
			i++
			hasValue = true
			if i < len(s) && s[i] == '"' {
				// quoted-string with backslash escapes
				i++
				var b strings.Builder
				closed := false
				for i < len(s) {
					c := s[i]
					if c == '\\' && i+1 < len(s) {
						b.WriteByte(s[i+1])
						i += 2
						continue
					}
					if c == '"' {
						i++
						closed = true
						break
					}
					b.WriteByte(c)
					i++
				}
				if !closed {
					return cc, false
				}
				value = b.String()
			} else {
				vs := i
				for i < len(s) && isTokenChar(s[i]) {
					i++
				}
				if i == vs {
					return cc, false // "=" with no token value
				}
				value = s[vs:i]
			}
		}
		// After a directive: OWS then "," or end.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i < len(s) && s[i] != ',' {
			return cc, false
		}
		switch name {
		case "no-store":
			cc.noStore = true
		case "no-cache":
			cc.noCache = true
		case "private":
			cc.private = true
		case "s-maxage", "max-age":
			if !hasValue {
				return cc, false
			}
			n, err := parseDeltaSeconds(value)
			if err {
				return cc, false
			}
			if name == "s-maxage" {
				cc.sMaxage = n
			} else {
				cc.maxAge = n
			}
		}
	}
	return cc, true
}

func parseDeltaSeconds(v string) (n int64, bad bool) {
	if v == "" {
		return 0, true
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, true
		}
		d := int64(v[i] - '0')
		if n > (1<<31)/10 { // clamp: huge ages saturate, they don't overflow
			n = 1 << 31
			continue
		}
		n = n*10 + d
	}
	return n, false
}

// freshnessFor applies the TTL rules: s-maxage wins, then max-age (each
// capped at ttl_max), else the site's ttl.
func freshnessFor(cc respCacheControl, siteTTL, ttlMax time.Duration) time.Duration {
	if cc.sMaxage >= 0 {
		return min(time.Duration(cc.sMaxage)*time.Second, ttlMax)
	}
	if cc.maxAge >= 0 {
		return min(time.Duration(cc.maxAge)*time.Second, ttlMax)
	}
	return siteTTL
}

// --- snapshot -------------------------------------------------------------------

// cacheStatsBucket is the JSON counter block, used for the process totals
// and per app.
type cacheStatsBucket struct {
	Hits             int64 `json:"hits"`
	Misses           int64 `json:"misses"`
	Coalesced        int64 `json:"coalesced"`
	Bypass           int64 `json:"bypass"`
	Stores           int64 `json:"stores"`
	Purges           int64 `json:"purges"`
	Evictions        int64 `json:"evictions"`
	FencedStores     int64 `json:"fenced_stores"`
	AdmissionRejects int64 `json:"admission_rejects"`
	WaiterOverflow   int64 `json:"waiter_overflow"`
	WaiterExpired    int64 `json:"waiter_expired"`
	Entries          int64 `json:"entries"`
	StoredBytes      int64 `json:"stored_bytes"`
}

func (b *cacheStatsBucket) add(c *cacheCounters) {
	b.Hits += c.hits.Load()
	b.Misses += c.misses.Load()
	b.Coalesced += c.coalesced.Load()
	b.Bypass += c.bypass.Load()
	b.Stores += c.stores.Load()
	b.Purges += c.purges.Load()
	b.Evictions += c.evictions.Load()
	b.FencedStores += c.fencedStores.Load()
	b.AdmissionRejects += c.admissionRejects.Load()
	b.WaiterOverflow += c.waiterOverflow.Load()
	b.WaiterExpired += c.waiterExpired.Load()
	b.Entries += c.entries.Load()
	b.StoredBytes += c.storedBytes.Load()
}

// cacheStats is the GET /1.0/cache response body.
type cacheStats struct {
	cacheStatsBucket
	Apps map[string]*cacheStatsBucket `json:"apps"`
}

// snapshot sums per-shard atomics; values are monotonic but not mutually
// atomic. No shard mutex is taken — a tight scrape loop cannot degrade
// the data plane.
func (c *cacheStore) snapshot() cacheStats {
	out := cacheStats{Apps: map[string]*cacheStatsBucket{}}
	for _, sh := range c.shards {
		out.add(&sh.ctr)
		sh.apps.Range(func(k, v any) bool {
			id := k.(string)
			b := out.Apps[id]
			if b == nil {
				b = &cacheStatsBucket{}
				out.Apps[id] = b
			}
			b.add(v.(*cacheCounters))
			return true
		})
	}
	return out
}
