package janus

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Heartbeat TTL (docs/20260719-002000-pool-protocol.md "Defaults": 5s / 15s).
// An app whose heartbeat clock is older than the TTL is dead — same effect
// as DELETE. Heartbeat ≠ readiness: the clock proves the supervising process
// is alive, independent of upstreams[].
const defaultHeartbeatTTL = 15 * time.Second

const minHeartbeatSweepInterval = time.Millisecond

// Keep the documented ttl/3 sweep cadence at or above the operational floor.
// The sweeper retains its own floor as defense against invalid direct state
// construction in tests or future internal callers.
const minHeartbeatTTL = 3 * minHeartbeatSweepInterval

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
	if d < minHeartbeatTTL {
		return 0, fmt.Errorf("%s must be at least %v, got %q", heartbeatTTLEnv, minHeartbeatTTL, v)
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
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Hosts     []string     `json:"hosts"`
	Upstreams []Upstream   `json:"upstreams"`
	Site      *SitePolicy  `json:"site,omitempty"`
	Files     *FilesPolicy `json:"files,omitempty"`
	Lease     string       `json:"lease"`

	// Bridge is the tenant's hub bridge endpoint (optional; empty =
	// hub handshakes answer 503). Cold config never carries it: which URL
	// the tenant serves is tenant knowledge, exactly like socket paths.
	Bridge string `json:"bridge,omitempty"`

	// heartbeatAt is the app's heartbeat clock. Registration stamps it
	// (heartbeats begin immediately after registration, so a slow cold
	// boot is never mistaken for dead); each POST …/heartbeat re-stamps.
	heartbeatAt time.Time

	// selectMu serializes least-connections selection only for this app.
	// Registry snapshots share the pointer, so unrelated tenants never
	// contend while a worker is selected and charged.
	selectMu *sync.Mutex

	// siteSuffix is the normalized suffix owned by Site. siteValue is the
	// request-local label resolved from a pattern or alias. Both stay
	// internal to the registry snapshot.
	siteSuffix string
	siteValue  string
	access     *accessState
}

func (rec *AppRecord) clone() AppRecord {
	out := *rec
	out.Hosts = append([]string{}, rec.Hosts...)
	out.Upstreams = append([]Upstream{}, rec.Upstreams...)
	out.Site = cloneSitePolicy(rec.Site)
	out.Files = cloneFilesPolicy(rec.Files)
	return out
}

func (rec AppRecord) concreteHosts() []string {
	out := append([]string{}, rec.Hosts...)
	if rec.Site != nil {
		for host := range rec.Site.Aliases {
			out = append(out, host)
		}
		sort.Strings(out)
	}
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

func errSiteConflict(format string, args ...any) *apiError {
	return &apiError{Status: http.StatusConflict, Msg: fmt.Sprintf(format, args...)}
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

// validateHostname checks the label grammar hostLabelRE describes with a
// single allocation-free byte scan: this runs on every data-plane request
// via resolveRequestHost, so it must never split or regexp-match.
func validateHostname(h string) error {
	if h == "" {
		return errBadRequest("host must not be empty")
	}
	if len(h) > 253 {
		return errBadRequest("host %q is too long (max 253 chars)", h)
	}
	labelLen := 0
	for i := 0; i < len(h); i++ {
		switch c := h[i]; {
		case c == '.':
			if labelLen == 0 || h[i-1] == '-' {
				return errBadRequest("host %q is not a plausible hostname", h)
			}
			labelLen = 0
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			labelLen++
		case c == '-':
			if labelLen == 0 {
				return errBadRequest("host %q is not a plausible hostname", h)
			}
			labelLen++
		default:
			return errBadRequest("host %q is not a plausible hostname", h)
		}
		if labelLen > 63 {
			return errBadRequest("host %q is not a plausible hostname", h)
		}
	}
	if labelLen == 0 || h[len(h)-1] == '-' {
		return errBadRequest("host %q is not a plausible hostname", h)
	}
	return nil
}

// normalizeBridge collapses leading/trailing slashes to exactly one
// leading slash: "hub", "/hub", "hub/", "///hub///" → "/hub".
func normalizeBridge(p string) (string, error) {
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	for strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	if p == "" {
		return "", errBadRequest("bridge is empty")
	}
	p = "/" + p
	if len(p) > 256 {
		return "", errBadRequest("bridge %q is too long (max 256 bytes)", p)
	}
	if strings.Contains(p, "//") {
		return "", errBadRequest("bridge %q must not contain empty path segments", p)
	}
	for _, r := range p {
		if r == '?' || r == '#' {
			return "", errBadRequest("bridge %q must not contain %q (it is a path, not a URL)", p, string(r))
		}
		if r <= ' ' || r == 0x7f {
			return "", errBadRequest("bridge %q must not contain whitespace or control characters", p)
		}
	}
	return p, nil
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
	mu     sync.RWMutex
	apps   map[string]*AppRecord // id → record
	hosts  map[string]string     // host → holding app id (first-wins)
	sites  map[string]string     // pattern suffix → holding app id
	browse *browseSupervisor
	access *accessBridge

	// hubTeardown tears the app's hub down on DELETE and TTL reap — the
	// only events that kill a registration. Upstreams PUTs never touch
	// hub state. Wired once by the pooled state holder.
	hubTeardown func(appID string)

	// hubHostsRemoved closes hub connections bound through hosts a PATCH
	// removed from the app; all other membership stays.
	hubHostsRemoved func(appID string, removed map[string]bool)

	// pruneUpstreams lets the data plane drop per-socket state for paths
	// no registered app references anymore; invoked (outside the lock)
	// after every event that retires upstream paths: upstreams PUT,
	// DELETE, and heartbeat reap.
	pruneUpstreams func()

	// mdnsNotify pings the mdns advertiser's reconcile loop after every
	// event that changes the registered host set: create, host-changing
	// PATCH, DELETE, and TTL reap. Invoked outside the lock; the hook is
	// a non-blocking channel send by construction — no registry mutation
	// ever waits on multicast I/O. Wired once by the pooled state holder.
	mdnsNotify func()

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
		sites: map[string]string{},
		now:   time.Now,
		ttl:   defaultHeartbeatTTL,
	}
}

// bindAccess installs the pooled bridge under the same lock registration
// creation uses to allocate access state. Overlapping generations must bind
// the same separately pooled bridge.
func (r *appRegistry) bindAccess(bridge *accessBridge) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.access != nil && r.access != bridge {
		return errors.New("janus access: registry already bound to another bridge")
	}
	r.access = bridge
	return nil
}

func (r *appRegistry) create(name string, hosts []string, bridgeVal string) (AppRecord, error) {
	return r.createWithLease(name, hosts, nil, nil, bridgeVal, nil, "heartbeat")
}

func (r *appRegistry) createWithPolicy(name string, hosts []string, site *SitePolicy, files *FilesPolicy, bridgeVal string) (AppRecord, error) {
	return r.createWithLease(name, hosts, site, files, bridgeVal, nil, "heartbeat")
}

func (r *appRegistry) createWithLease(name string, hosts []string, site *SitePolicy, files *FilesPolicy, bridgeVal string, upstreams []Upstream, lease string) (AppRecord, error) {
	if err := validateAppName(name); err != nil {
		return AppRecord{}, err
	}
	if (len(hosts) > 0) == (site != nil) {
		return AppRecord{}, errBadRequest("exactly one of hosts or site is required")
	}
	var err error
	var suffix string
	if site != nil {
		site, suffix, err = normalizeSitePolicy(site)
	} else {
		hosts, err = normalizeHosts(hosts)
	}
	if err != nil {
		return AppRecord{}, err
	}
	files, err = normalizeFilesPolicy(files, site != nil)
	if err != nil {
		return AppRecord{}, err
	}
	if bridgeVal != "" {
		bridgeVal, err = normalizeBridge(bridgeVal)
		if err != nil {
			return AppRecord{}, err
		}
	}
	if err := validateUpstreams(upstreams); err != nil {
		return AppRecord{}, err
	}
	if lease != "heartbeat" && lease != "process" {
		return AppRecord{}, errBadRequest(`lease must be "heartbeat" or "process"`)
	}
	if files != nil && files.Shell == "" {
		allBrowse := len(files.Roots) > 0
		for _, root := range files.Roots {
			allBrowse = allBrowse && root.Browse
		}
		if !allBrowse || len(files.ProxyFirst) != 0 || len(upstreams) != 0 {
			return AppRecord{}, errBadRequest("files.shell may be omitted only for a terminal browse-only registration")
		}
	}

	r.mu.Lock()
	claims := append([]string(nil), hosts...)
	if site != nil {
		for alias := range site.Aliases {
			claims = append(claims, alias)
		}
	}
	for _, h := range claims {
		if r.browse != nil && r.browse.coldClaim(h) {
			r.mu.Unlock()
			return AppRecord{}, errHostConflict(h, "cold:"+h)
		}
		if holder, taken := r.hosts[h]; taken {
			r.mu.Unlock()
			return AppRecord{}, errHostConflict(h, holder)
		}
		if heldSuffix, holder, taken := r.patternClaimForExactLocked(h); taken {
			r.mu.Unlock()
			return AppRecord{}, errSiteConflict("host %q conflicts with site pattern suffix %q held by app %q", h, heldSuffix, holder)
		}
	}
	if suffix != "" {
		if r.browse != nil {
			if host, conflict := r.browse.coldHostUnderSuffix(suffix); conflict {
				r.mu.Unlock()
				return AppRecord{}, errSiteConflict("site pattern suffix %q conflicts with cold host %q", suffix, host)
			}
		}
		if held, holder, conflict := r.patternConflictLocked(suffix); conflict {
			r.mu.Unlock()
			return AppRecord{}, errSiteConflict("site pattern suffix %q conflicts with suffix %q held by app %q", suffix, held, holder)
		}
		for host, holder := range r.hosts {
			if hostUnderSuffix(host, suffix) {
				r.mu.Unlock()
				return AppRecord{}, errSiteConflict("site pattern suffix %q conflicts with host %q held by app %q", suffix, host, holder)
			}
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
	rec := &AppRecord{
		ID: id, Name: name, Hosts: hosts, Upstreams: append([]Upstream{}, upstreams...),
		Site: site, Files: files, Bridge: bridgeVal, Lease: lease,
		selectMu: new(sync.Mutex), siteSuffix: suffix,
	}
	if r.access != nil {
		rec.access = r.access.newState()
	}
	if lease == "heartbeat" {
		rec.heartbeatAt = r.now()
	}
	r.apps[id] = rec
	for _, h := range hosts {
		r.hosts[h] = id
	}
	if site != nil {
		r.sites[suffix] = id
		for alias := range site.Aliases {
			r.hosts[alias] = id
		}
	}
	out := rec.clone()
	r.mu.Unlock()
	if r.mdnsNotify != nil {
		r.mdnsNotify()
	}
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

func hostUnderSuffix(host, suffix string) bool {
	return strings.HasSuffix(host, "."+suffix)
}

func suffixesOverlap(a, b string) bool {
	return a == b || hostUnderSuffix(a, b) || hostUnderSuffix(b, a)
}

func (r *appRegistry) patternClaimForExactLocked(host string) (suffix, holder string, ok bool) {
	for suffix, holder := range r.sites {
		if hostUnderSuffix(host, suffix) {
			return suffix, holder, true
		}
	}
	return "", "", false
}

func (r *appRegistry) patternConflictLocked(suffix string) (held, holder string, ok bool) {
	for held, holder := range r.sites {
		if suffixesOverlap(suffix, held) {
			return held, holder, true
		}
	}
	return "", "", false
}

func (r *appRegistry) hostConflictLocked(host string) string {
	if holder := r.hosts[host]; holder != "" {
		return holder
	}
	_, holder, _ := r.patternClaimForExactLocked(host)
	return holder
}

func siteDirectoryExists(dir, site string) bool {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false
	}
	defer root.Close()
	info, err := root.Lstat(site)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

// resolveHost maps a public host to its app record (data-plane lookup).
// The returned record is a shallow snapshot sharing the record's slice
// backing arrays: every registry write replaces Hosts and Upstreams
// wholesale (create, patch, setUpstreams — never an in-place append), so a
// published backing array is immutable. Callers read, never mutate.
func (r *appRegistry) resolveHost(host string) (AppRecord, bool) {
	r.mu.RLock()
	id, ok := r.hosts[host]
	var site string
	if ok {
		rec := r.apps[id]
		if rec.Site != nil {
			site = rec.Site.Aliases[host]
		}
		out := *rec
		out.siteValue = site
		r.mu.RUnlock()
		if site != "" && !siteDirectoryExists(out.Site.Dir, site) {
			return AppRecord{}, false
		}
		return out, true
	}
	if net.ParseIP(host) != nil {
		r.mu.RUnlock()
		return AppRecord{}, false
	}
	var rec *AppRecord
	for suffix, appID := range r.sites {
		if !hostUnderSuffix(host, suffix) {
			continue
		}
		capture := strings.TrimSuffix(host, "."+suffix)
		if strings.Contains(capture, ".") || validateSiteLabel(capture, "site") != nil {
			continue
		}
		rec = r.apps[appID]
		site = capture
		break
	}
	if rec == nil {
		r.mu.RUnlock()
		return AppRecord{}, false
	}
	out := *rec
	out.siteValue = site
	r.mu.RUnlock()
	if !siteDirectoryExists(out.Site.Dir, site) {
		return AppRecord{}, false
	}
	return out, true
}

// resolveRequestHost normalizes exact and pattern hosts only after requiring
// a syntactically valid DNS authority.
func (r *appRegistry) resolveRequestHost(authority string) (AppRecord, bool) {
	host := authority
	bracketed := strings.HasPrefix(authority, "[")
	if strings.Contains(authority, ":") {
		var port string
		var err error
		host, port, err = net.SplitHostPort(authority)
		if err != nil || port == "" {
			return AppRecord{}, false
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return AppRecord{}, false
			}
		}
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return AppRecord{}, false
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ip := net.ParseIP(host)
	if bracketed && ip == nil {
		return AppRecord{}, false
	}
	if ip == nil && validateHostname(host) != nil {
		return AppRecord{}, false
	}
	return r.resolveHost(host)
}

// exists reports whether the app id is currently registered (the hub
// set's liveness oracle for zombie-free lazy construction).
func (r *appRegistry) exists(id string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.apps[id]
	return ok
}

// allUpstreamPaths is the data plane's prune oracle: every socket path
// any registered app currently references.
func (r *appRegistry) allUpstreamPaths() map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]bool{}
	for _, rec := range r.apps {
		for _, u := range rec.Upstreams {
			out[u.Path] = true
		}
	}
	return out
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

// patch updates name, hosts, and/or bridge; nil means "leave
// unchanged" (bridgeValSet with an empty value clears the path).
func (r *appRegistry) patch(id string, name *string, hosts *[]string, bridgeVal *string) (AppRecord, error) {
	if name == nil && hosts == nil && bridgeVal == nil {
		return AppRecord{}, errBadRequest("nothing to update (want name, hosts, and/or bridge)")
	}
	if name != nil {
		if err := validateAppName(*name); err != nil {
			return AppRecord{}, err
		}
	}
	if bridgeVal != nil && *bridgeVal != "" {
		normalized, err := normalizeBridge(*bridgeVal)
		if err != nil {
			return AppRecord{}, err
		}
		*bridgeVal = normalized
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
		if rec.Site != nil {
			r.mu.Unlock()
			return AppRecord{}, errBadRequest("a site-pattern app does not support hosts PATCH")
		}
		for _, h := range newHosts {
			if r.browse != nil && r.browse.coldClaim(h) {
				r.mu.Unlock()
				return AppRecord{}, errHostConflict(h, "cold:"+h)
			}
			if holder, taken := r.hosts[h]; taken && holder != id {
				r.mu.Unlock()
				return AppRecord{}, errHostConflict(h, holder)
			}
			if suffix, holder, taken := r.patternClaimForExactLocked(h); taken {
				r.mu.Unlock()
				return AppRecord{}, errSiteConflict("host %q conflicts with site pattern suffix %q held by app %q", h, suffix, holder)
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
	if bridgeVal != nil {
		rec.Bridge = *bridgeVal
	}
	out := rec.clone()
	r.mu.Unlock()
	// Removed hosts stop resolving to the app: their hub connections
	// close through the internal mechanism (all other membership stays).
	if len(removed) > 0 && r.hubHostsRemoved != nil {
		r.hubHostsRemoved(id, removed)
	}
	if hosts != nil && r.mdnsNotify != nil {
		r.mdnsNotify()
	}
	return out, nil
}

// setUpstreams replaces the entire upstream list atomically.
// Empty list is legal: registered but not routable.
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
	if rec.Files != nil && rec.Files.Shell == "" && len(ups) != 0 {
		r.mu.Unlock()
		return AppRecord{}, errBadRequest("terminal browse-only registration cannot publish upstreams")
	}
	rec.Upstreams = append([]Upstream{}, ups...)
	out := rec.clone()
	r.mu.Unlock()
	if r.pruneUpstreams != nil {
		r.pruneUpstreams()
	}
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
	if rec.Lease == "process" {
		return errBadRequest("process lease does not accept heartbeats")
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
	var detached []*accessSubscriber
	for id, rec := range r.apps {
		if rec.Lease == "process" {
			continue
		}
		if now.Sub(rec.heartbeatAt) > r.ttl {
			detached = append(detached, r.removeLocked(rec, "registration_reaped")...)
			reaped = append(reaped, id)
		}
	}
	r.mu.Unlock()
	for _, sub := range detached {
		close(sub.done)
	}
	for _, id := range reaped {
		// Reap kills the registration itself: the hub dies with it.
		if r.hubTeardown != nil {
			r.hubTeardown(id)
		}
	}
	if len(reaped) > 0 && r.pruneUpstreams != nil {
		r.pruneUpstreams()
	}
	if len(reaped) > 0 && r.mdnsNotify != nil {
		r.mdnsNotify()
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
		interval := r.ttl / 3
		if interval < minHeartbeatSweepInterval {
			interval = minHeartbeatSweepInterval
		}
		ticker := time.NewTicker(interval)
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
	detached := r.removeLocked(rec, "registration_deleted")
	r.mu.Unlock()
	for _, sub := range detached {
		close(sub.done)
	}
	// DELETE kills the registration itself: the hub dies with it.
	if r.hubTeardown != nil {
		r.hubTeardown(id)
	}
	if r.pruneUpstreams != nil {
		r.pruneUpstreams()
	}
	if r.mdnsNotify != nil {
		r.mdnsNotify()
	}
	return nil
}

// removeLocked is the one registration-removal cut shared by DELETE, reap,
// and final pooled-state destruction. Caller holds r.mu.
func (r *appRegistry) removeLocked(rec *AppRecord, reason string) []*accessSubscriber {
	for _, h := range rec.Hosts {
		delete(r.hosts, h)
	}
	if rec.Site != nil {
		delete(r.sites, rec.siteSuffix)
		for alias := range rec.Site.Aliases {
			delete(r.hosts, alias)
		}
	}
	delete(r.apps, rec.ID)
	if rec.access == nil {
		return nil
	}
	rec.access.mu.Lock()
	detached := rec.access.tombstoneLocked(reason)
	rec.access.mu.Unlock()
	return detached
}

func (r *appRegistry) tombstoneAll(reason string) []*accessSubscriber {
	r.mu.Lock()
	var detached []*accessSubscriber
	records := make([]*AppRecord, 0, len(r.apps))
	for _, rec := range r.apps {
		records = append(records, rec)
	}
	for _, rec := range records {
		detached = append(detached, r.removeLocked(rec, reason)...)
	}
	r.mu.Unlock()
	return detached
}

// heartbeatAges reports each registered app's age since its last
// heartbeat — the mdns status surface's freshness read (an age, never a
// wall-clock timestamp; /1.0/apps stays unchanged). Read lock only.
func (r *appRegistry) heartbeatAges() map[string]time.Duration {
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]time.Duration, len(r.apps))
	for id, rec := range r.apps {
		if rec.Lease == "heartbeat" {
			out[id] = now.Sub(rec.heartbeatAt)
		}
	}
	return out
}

// --- HTTP handlers ---------------------------------------------------------

func (a *App) appsRegistry() *appRegistry { return a.appsReg }

type appCreateRequest struct {
	Name      string          `json:"name"`
	Hosts     json.RawMessage `json:"hosts"`
	Site      json.RawMessage `json:"site"`
	Files     json.RawMessage `json:"files"`
	Upstreams json.RawMessage `json:"upstreams"`
	Bridge    string          `json:"bridge"`
	Lease     json.RawMessage `json:"lease"`
}

type appPatchRequest struct {
	Name  *string   `json:"name"`
	Hosts *[]string `json:"hosts"`

	// Bridge is tri-state: absent = unchanged, null = clear, string =
	// set (validated). json.RawMessage distinguishes absent from null.
	Bridge json.RawMessage `json:"bridge"`
}

// bridgeValPatch decodes the tri-state bridge field: nil pointer =
// leave unchanged; empty string = clear; value = set.
func bridgeValPatch(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	if string(raw) == "null" {
		empty := ""
		return &empty, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errBadRequest("bridge must be a string or null")
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
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
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
	if (req.Hosts == nil) == (req.Site == nil) {
		writeAPIError(w, errBadRequest("exactly one of hosts or site is required"))
		return
	}
	if string(req.Hosts) == "null" || string(req.Site) == "null" || string(req.Files) == "null" {
		writeAPIError(w, errBadRequest("hosts, site, and files must not be null"))
		return
	}
	var hosts []string
	var site *SitePolicy
	var files *FilesPolicy
	var upstreams []Upstream
	if req.Hosts != nil {
		if err := json.Unmarshal(req.Hosts, &hosts); err != nil {
			writeAPIError(w, errBadRequest("hosts must be an array of strings"))
			return
		}
	}
	if req.Site != nil {
		var value SitePolicy
		if err := decodeStrictRaw(req.Site, &value); err != nil {
			writeAPIError(w, errBadRequest("invalid site: %v", err))
			return
		}
		site = &value
	}
	if req.Files != nil {
		var value FilesPolicy
		if err := decodeStrictRaw(req.Files, &value); err != nil {
			writeAPIError(w, errBadRequest("invalid files: %v", err))
			return
		}
		files = &value
	}
	if req.Upstreams != nil {
		if string(req.Upstreams) == "null" {
			writeAPIError(w, errBadRequest("upstreams must be an array"))
			return
		}
		if err := decodeStrictRaw(req.Upstreams, &upstreams); err != nil {
			writeAPIError(w, errBadRequest("invalid upstreams: %v", err))
			return
		}
	}
	lease := "heartbeat"
	if req.Lease != nil {
		if string(req.Lease) == "null" {
			writeAPIError(w, errBadRequest("lease must not be null"))
			return
		}
		if err := json.Unmarshal(req.Lease, &lease); err != nil || lease == "" {
			writeAPIError(w, errBadRequest(`lease must be "heartbeat" or "process"`))
			return
		}
	}
	rec, err := a.appsRegistry().createWithLease(
		req.Name, hosts, site, files, req.Bridge, upstreams, lease,
	)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": rec.ID})
}

func decodeStrictRaw(raw json.RawMessage, value any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
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
	bp, err := bridgeValPatch(req.Bridge)
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
