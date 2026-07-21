package janus

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Heartbeat TTL (docs/20260719-002000-pool-protocol.md "Defaults": 5s / 15s).
// An app whose heartbeat clock is older than the TTL is dead — same effect
// as DELETE. Heartbeat ≠ readiness: the clock proves the supervising process
// is alive, independent of upstreams[].
const defaultHeartbeatTTL = 15 * time.Second

// heartbeatTTLEnv lets a test harness shorten the TTL. Unset in production.
const heartbeatTTLEnv = "JANUS_HEARTBEAT_TTL"

// heartbeatTTLFromEnv resolves the TTL, rejecting illegal values loudly.
func heartbeatTTLFromEnv() (time.Duration, error) {
	v := os.Getenv(heartbeatTTLEnv)
	if v == "" {
		return defaultHeartbeatTTL, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a Go duration (want e.g. \"15s\"): %v", heartbeatTTLEnv, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", heartbeatTTLEnv, v)
	}
	return d, nil
}

// Upstream is one entry in an app's upstream list.
type Upstream struct {
	// Path is the unix socket path Janus may dial.
	Path string `json:"path"`

	// Doorbell marks the tenant's wake-up socket. A doorbell entry must
	// be the only entry in the list. Phase 3 stores and validates the
	// flag; ringing is data-plane behavior (Phase 4).
	Doorbell bool `json:"doorbell,omitempty"`
}

// AppRecord is one registered app in the hot registry.
type AppRecord struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Hosts     []string   `json:"hosts"`
	Upstreams []Upstream `json:"upstreams"`

	// BridgePath is the tenant's hub bridge endpoint (optional; empty =
	// hub handshakes answer 503). Cold config never carries it: which URL
	// the tenant serves is tenant knowledge, exactly like socket paths.
	BridgePath string `json:"bridge_path,omitempty"`

	// heartbeatAt is the app's heartbeat clock. Registration stamps it
	// (heartbeats begin immediately after registration, so a slow cold
	// boot is never mistaken for dead); each POST …/heartbeat re-stamps.
	heartbeatAt time.Time

	// gen is the app's cache generation counter
	// (docs/20260720-033201-capability-microcache.md "purge on swap,
	// generation-fenced"). Every purge event — upstreams PUT, DELETE,
	// heartbeat reap, host claim — bumps it inside the registry's
	// critical section; cache fills snapshot it with host resolution and
	// the store is rejected on mismatch. Pointer, so registry snapshots
	// share the live counter.
	gen *atomic.Uint64

	// genSnap is the generation observed by resolveHost, read under the
	// same RLock as the Upstreams snapshot so the two are mutually
	// consistent — a fill's snapshot can never pair a post-swap
	// generation with pre-swap sockets.
	genSnap uint64
}

func (rec *AppRecord) clone() AppRecord {
	out := *rec
	out.Hosts = append([]string{}, rec.Hosts...)
	out.Upstreams = append([]Upstream{}, rec.Upstreams...)
	return out
}

// apiError carries an HTTP status with a precise message.
type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return e.Msg }

func errBadRequest(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

func errUnknownApp(id string) *apiError {
	return &apiError{Status: http.StatusNotFound, Msg: fmt.Sprintf("unknown app id %q", id)}
}

func errHostConflict(host, holder string) *apiError {
	return &apiError{
		Status: http.StatusConflict,
		Msg:    fmt.Sprintf("host %q is already claimed by app %q", host, holder),
	}
}

// --- validation ------------------------------------------------------------

var (
	appNameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

func validateAppName(name string) error {
	if name == "" {
		return errBadRequest("name is required")
	}
	if !appNameRE.MatchString(name) {
		return errBadRequest("invalid name %q (want lowercase letters, digits, and interior hyphens; max 63 chars)", name)
	}
	return nil
}

// normalizeHosts lowercases (hostnames are case-insensitive), validates each
// host, and rejects duplicates within the request.
func normalizeHosts(hosts []string) ([]string, error) {
	if len(hosts) == 0 {
		return nil, errBadRequest("hosts is required and must not be empty")
	}
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		n := strings.ToLower(h)
		if err := validateHostname(n); err != nil {
			return nil, err
		}
		if seen[n] {
			return nil, errBadRequest("duplicate host %q in request", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

func validateHostname(h string) error {
	if h == "" {
		return errBadRequest("host must not be empty")
	}
	if len(h) > 253 {
		return errBadRequest("host %q is too long (max 253 chars)", h)
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) > 63 || !hostLabelRE.MatchString(label) {
			return errBadRequest("host %q is not a plausible hostname", h)
		}
	}
	return nil
}

// validateBridgePath checks the hub bridge endpoint: a /-prefixed path,
// ≤256 bytes, no whitespace or control characters, no ? or # (it is a
// path, not a URL).
func validateBridgePath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return errBadRequest("bridge_path %q must start with /", p)
	}
	if len(p) > 256 {
		return errBadRequest("bridge_path %q is too long (max 256 bytes)", p)
	}
	for _, r := range p {
		if r == '?' || r == '#' {
			return errBadRequest("bridge_path %q must not contain %q (it is a path, not a URL)", p, string(r))
		}
		if r <= ' ' || r == 0x7f {
			return errBadRequest("bridge_path %q must not contain whitespace or control characters", p)
		}
	}
	return nil
}

func validateUpstreams(ups []Upstream) error {
	doorbells := 0
	seen := map[string]bool{}
	for _, u := range ups {
		if u.Path == "" {
			return errBadRequest("upstream path is required")
		}
		if seen[u.Path] {
			return errBadRequest("duplicate upstream path %q", u.Path)
		}
		seen[u.Path] = true
		if u.Doorbell {
			doorbells++
		}
	}
	if doorbells > 1 {
		return errBadRequest("at most one doorbell entry is allowed, got %d", doorbells)
	}
	if doorbells == 1 && len(ups) > 1 {
		return errBadRequest("a doorbell entry must be the only entry, got %d entries", len(ups))
	}
	return nil
}

// --- id minting ------------------------------------------------------------

const (
	idSuffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	idSuffixLen      = 6
)

func mintIDSuffix() (string, error) {
	b := make([]byte, idSuffixLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = idSuffixAlphabet[int(b[i])%len(idSuffixAlphabet)]
	}
	return string(b), nil
}

// --- registry --------------------------------------------------------------

// appRegistry is the memory-only apps registry. Janus restart → empty;
// tenants re-register. Reads share the lock; writes are exclusive.
type appRegistry struct {
	mu    sync.RWMutex
	apps  map[string]*AppRecord // id → record
	hosts map[string]string     // host → holding app id (first-wins)

	// purge is the cache's purge hook, invoked (outside the registry
	// lock, after the generation bump inside it) on every purge event:
	// upstreams PUT, DELETE, heartbeat reap, and host claim. Atomic
	// because each config generation re-points it at its own cache store
	// while the pooled registry keeps serving. Nil when no cache store is
	// wired (tests).
	purge atomic.Pointer[func(appID string)]

	// hubTeardown tears the app's hub down on DELETE and TTL reap — the
	// only events that kill a registration. Upstreams PUTs never touch
	// hub state. Wired once by the pooled state holder.
	hubTeardown func(appID string)

	// hubHostsRemoved closes hub connections bound through hosts a PATCH
	// removed from the app; all other membership stays.
	hubHostsRemoved func(appID string, removed map[string]bool)

	// now is the heartbeat clock source; tests inject a fake.
	now func() time.Time

	// ttl is the heartbeat TTL; the background sweep reaps older clocks.
	ttl time.Duration

	sweepStop chan struct{}
	sweepDone chan struct{}
}

func newAppRegistry() *appRegistry {
	return &appRegistry{
		apps:  map[string]*AppRecord{},
		hosts: map[string]string{},
		now:   time.Now,
		ttl:   defaultHeartbeatTTL,
	}
}

// setPurge points the purge hook at the current config generation's cache
// store; assign-only, never unset, so a reload can never orphan purges.
func (r *appRegistry) setPurge(fn func(appID string)) {
	r.purge.Store(&fn)
}

func (r *appRegistry) purgeApp(id string) {
	if fn := r.purge.Load(); fn != nil {
		(*fn)(id)
	}
}

func (r *appRegistry) create(name string, hosts []string, bridgePath string) (AppRecord, error) {
	if err := validateAppName(name); err != nil {
		return AppRecord{}, err
	}
	hosts, err := normalizeHosts(hosts)
	if err != nil {
		return AppRecord{}, err
	}
	if bridgePath != "" {
		if err := validateBridgePath(bridgePath); err != nil {
			return AppRecord{}, err
		}
	}

	r.mu.Lock()
	for _, h := range hosts {
		if holder, taken := r.hosts[h]; taken {
			r.mu.Unlock()
			return AppRecord{}, errHostConflict(h, holder)
		}
	}
	var id string
	for {
		suffix, err := mintIDSuffix()
		if err != nil {
			r.mu.Unlock()
			return AppRecord{}, fmt.Errorf("minting app id: %w", err)
		}
		id = name + "-" + suffix
		if _, exists := r.apps[id]; !exists {
			break
		}
	}
	// Registration counts as the first heartbeat.
	rec := &AppRecord{ID: id, Name: name, Hosts: hosts, Upstreams: []Upstream{}, BridgePath: bridgePath, heartbeatAt: r.now(), gen: new(atomic.Uint64)}
	// Host claim is a purge event (docs/20260720-033201-capability-microcache.md
	// O5): bump the (fresh) generation in the same critical section as the
	// registry write; the entry drop below runs after the lock releases.
	rec.gen.Add(1)
	r.apps[id] = rec
	for _, h := range hosts {
		r.hosts[h] = id
	}
	out := rec.clone()
	r.mu.Unlock()
	r.purgeApp(id)
	return out, nil
}

func (r *appRegistry) list() []AppRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AppRecord, 0, len(r.apps))
	for _, rec := range r.apps {
		out = append(out, rec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// resolveHost maps a public host to its app record (data-plane lookup).
// The returned record is a shallow snapshot sharing the record's slice
// backing arrays: every registry write replaces Hosts and Upstreams
// wholesale (create, patch, setUpstreams — never an in-place append), so a
// published backing array is immutable. Callers read, never mutate.
func (r *appRegistry) resolveHost(host string) (AppRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.hosts[host]
	if !ok {
		return AppRecord{}, false
	}
	rec := *r.apps[id]
	rec.genSnap = rec.gen.Load()
	return rec, true
}

func (r *appRegistry) get(id string) (AppRecord, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.apps[id]
	if !ok {
		return AppRecord{}, errUnknownApp(id)
	}
	return rec.clone(), nil
}

// patch updates name, hosts, and/or bridge_path; nil means "leave
// unchanged" (bridgePathSet with an empty value clears the path).
func (r *appRegistry) patch(id string, name *string, hosts *[]string, bridgePath *string) (AppRecord, error) {
	if name == nil && hosts == nil && bridgePath == nil {
		return AppRecord{}, errBadRequest("nothing to update (want name, hosts, and/or bridge_path)")
	}
	if name != nil {
		if err := validateAppName(*name); err != nil {
			return AppRecord{}, err
		}
	}
	if bridgePath != nil && *bridgePath != "" {
		if err := validateBridgePath(*bridgePath); err != nil {
			return AppRecord{}, err
		}
	}
	var newHosts []string
	if hosts != nil {
		var err error
		newHosts, err = normalizeHosts(*hosts)
		if err != nil {
			return AppRecord{}, err
		}
	}

	r.mu.Lock()
	rec, ok := r.apps[id]
	if !ok {
		r.mu.Unlock()
		return AppRecord{}, errUnknownApp(id)
	}
	var removed map[string]bool
	if hosts != nil {
		for _, h := range newHosts {
			if holder, taken := r.hosts[h]; taken && holder != id {
				r.mu.Unlock()
				return AppRecord{}, errHostConflict(h, holder)
			}
		}
		kept := map[string]bool{}
		for _, h := range newHosts {
			kept[h] = true
		}
		removed = map[string]bool{}
		for _, h := range rec.Hosts {
			delete(r.hosts, h)
			if !kept[h] {
				removed[h] = true
			}
		}
		for _, h := range newHosts {
			r.hosts[h] = id
		}
		rec.Hosts = newHosts
	}
	if name != nil {
		rec.Name = *name
	}
	if bridgePath != nil {
		rec.BridgePath = *bridgePath
	}
	out := rec.clone()
	r.mu.Unlock()
	// Removed hosts stop resolving to the app: their hub connections
	// close through the internal mechanism (all other membership stays).
	if len(removed) > 0 && r.hubHostsRemoved != nil {
		r.hubHostsRemoved(id, removed)
	}
	return out, nil
}

// setUpstreams replaces the entire upstream list atomically.
// Empty list is legal: registered but not routable.
//
// Every upstreams PUT is a cache purge event: the generation bump shares
// the registry write's critical section (the admission cut also cuts the
// cache), and the entry drop follows outside the lock. With staggered
// publishes a w:16 boot is 16 purges — the cache stays cold through
// warmup, by design; any future scheme that diffs socket lists to skip
// "redundant" purges must re-derive the spec's race analysis first.
func (r *appRegistry) setUpstreams(id string, ups []Upstream) (AppRecord, error) {
	if err := validateUpstreams(ups); err != nil {
		return AppRecord{}, err
	}
	r.mu.Lock()
	rec, ok := r.apps[id]
	if !ok {
		r.mu.Unlock()
		return AppRecord{}, errUnknownApp(id)
	}
	rec.Upstreams = append([]Upstream{}, ups...)
	rec.gen.Add(1)
	out := rec.clone()
	r.mu.Unlock()
	r.purgeApp(id)
	return out, nil
}

// heartbeat stamps the app's heartbeat clock.
func (r *appRegistry) heartbeat(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.apps[id]
	if !ok {
		return errUnknownApp(id)
	}
	rec.heartbeatAt = r.now()
	return nil
}

// sweepExpired reaps every app whose heartbeat clock is older than the TTL
// and returns the reaped ids. Same effect as delete: entry removed, hosts
// freed; the tenant must re-register. Heartbeat ≠ readiness — empty
// upstreams with a fresh clock stays registered; only a stale clock kills.
// All state is per app id, so one expiring app never touches another.
func (r *appRegistry) sweepExpired() []string {
	now := r.now()
	r.mu.Lock()
	var reaped []string
	for id, rec := range r.apps {
		if now.Sub(rec.heartbeatAt) > r.ttl {
			for _, h := range rec.Hosts {
				delete(r.hosts, h)
			}
			delete(r.apps, id)
			rec.gen.Add(1) // reap is a purge event
			reaped = append(reaped, id)
		}
	}
	r.mu.Unlock()
	for _, id := range reaped {
		r.purgeApp(id)
		// Reap kills the registration itself: the hub dies with it.
		if r.hubTeardown != nil {
			r.hubTeardown(id)
		}
	}
	return reaped
}

// startSweeper runs the background TTL sweep on a ticker at TTL/3.
func (r *appRegistry) startSweeper(logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}
	r.sweepStop = make(chan struct{})
	r.sweepDone = make(chan struct{})
	go func() {
		defer close(r.sweepDone)
		ticker := time.NewTicker(r.ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, id := range r.sweepExpired() {
					logger.Warn("janus app heartbeat TTL expired; registration reaped",
						zap.String("app", id),
						zap.Duration("ttl", r.ttl),
					)
				}
			case <-r.sweepStop:
				return
			}
		}
	}()
}

// stopSweeper stops the sweep and waits for its goroutine to exit.
func (r *appRegistry) stopSweeper() {
	if r.sweepStop == nil {
		return
	}
	close(r.sweepStop)
	<-r.sweepDone
	r.sweepStop = nil
}

func (r *appRegistry) delete(id string) error {
	r.mu.Lock()
	rec, ok := r.apps[id]
	if !ok {
		r.mu.Unlock()
		return errUnknownApp(id)
	}
	for _, h := range rec.Hosts {
		delete(r.hosts, h)
	}
	delete(r.apps, id)
	rec.gen.Add(1) // delete is a purge event
	r.mu.Unlock()
	r.purgeApp(id)
	// DELETE kills the registration itself: the hub dies with it.
	if r.hubTeardown != nil {
		r.hubTeardown(id)
	}
	return nil
}

// --- HTTP handlers ---------------------------------------------------------

func (a *App) appsRegistry() *appRegistry { return a.appsReg }

type appCreateRequest struct {
	Name       string   `json:"name"`
	Hosts      []string `json:"hosts"`
	BridgePath string   `json:"bridge_path"`
}

type appPatchRequest struct {
	Name  *string   `json:"name"`
	Hosts *[]string `json:"hosts"`

	// BridgePath is tri-state: absent = unchanged, null = clear, string =
	// set (validated). json.RawMessage distinguishes absent from null.
	BridgePath json.RawMessage `json:"bridge_path"`
}

// bridgePathPatch decodes the tri-state bridge_path field: nil pointer =
// leave unchanged; empty string = clear; value = set.
func bridgePathPatch(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	if string(raw) == "null" {
		empty := ""
		return &empty, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errBadRequest("bridge_path must be a string or null")
	}
	return &s, nil
}

type upstreamsPutRequest struct {
	Upstreams *[]Upstream `json:"upstreams"`
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errBadRequest("malformed JSON body: %v", err)
	}
	if dec.More() {
		return errBadRequest("malformed JSON body: trailing data")
	}
	return nil
}

func writeAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = &apiError{Status: http.StatusInternalServerError, Msg: err.Error()}
	}
	writeJSON(w, ae.Status, map[string]string{"error": ae.Msg})
}

func (a *App) handleAppsCreate(w http.ResponseWriter, r *http.Request) {
	var req appCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	rec, err := a.appsRegistry().create(req.Name, req.Hosts, req.BridgePath)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": rec.ID})
}

func (a *App) handleAppsList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.appsRegistry().list())
}

func (a *App) handleAppsGet(w http.ResponseWriter, r *http.Request) {
	rec, err := a.appsRegistry().get(r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleAppsPatch(w http.ResponseWriter, r *http.Request) {
	var req appPatchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	bp, err := bridgePathPatch(req.BridgePath)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	rec, err := a.appsRegistry().patch(r.PathValue("id"), req.Name, req.Hosts, bp)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleAppsDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.appsRegistry().delete(r.PathValue("id")); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleAppsHeartbeat(w http.ResponseWriter, r *http.Request) {
	// The protocol heartbeat is bodyless; reject anything else loudly.
	var probe [1]byte
	if n, _ := r.Body.Read(probe[:]); n > 0 {
		writeAPIError(w, errBadRequest("heartbeat takes no body"))
		return
	}
	if err := a.appsRegistry().heartbeat(r.PathValue("id")); err != nil {
		writeAPIError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleAppsUpstreamsPut(w http.ResponseWriter, r *http.Request) {
	var req upstreamsPutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Upstreams == nil {
		writeAPIError(w, errBadRequest("upstreams is required (empty list means not routable)"))
		return
	}
	rec, err := a.appsRegistry().setUpstreams(r.PathValue("id"), *req.Upstreams)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
