package janus

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brutella/dnssd"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// The mdns advertiser and front door
// (docs/20260722-034619-capability-mdns.md). The advertiser lives in the
// pooled process state beside registry, data plane, and hubs: a config
// reload with mdns unchanged reconciles to a zero diff and never
// multicasts a goodbye or re-probe. All multicast I/O happens on the one
// reconcile goroutine — registry mutations only ping it through a
// non-blocking notification hook.

// mdnsBlockedNets is the address block list applied to every service
// registration when interfaces are auto-selected or pinned: loopback and
// IPv4 link-local never appear in answers; IPv6 link-local (fe80::/10)
// is legitimate mDNS material and is advertised.
var mdnsBlockedNets = []string{"127.0.0.0/8", "::1/128", "169.254.0.0/16"}

// Advertised-entry states — the pinned /1.0/mdns enum.
const (
	mdnsStateProbing   = "probing"
	mdnsStateAnnounced = "announced"
	mdnsStateRenamed   = "renamed"
)

// Service types carried by the two registration shapes.
const (
	mdnsTypeFrontDoor = "_http._tcp"  // the configured name; port = front-door port
	mdnsTypeAppHost   = "_https._tcp" // registered app hosts; port 443, advisory
)

// mdnsAdvertisableHost reports whether a registered host has the mDNS
// host shape: exactly one label plus ".local". (Registry hosts arrive
// validated and lowercased.)
func mdnsAdvertisableHost(h string) bool {
	label, ok := strings.CutSuffix(h, ".local")
	return ok && label != "" && !strings.Contains(label, ".")
}

// mdnsSkippedHost reports whether a registered host counts in the
// skipped_hosts gauge: a .local name that is not advertisable
// (multi-label). Non-.local hosts are never counted — carrying a public
// DNS name is normal, not surprising.
func mdnsSkippedHost(h string) bool {
	return strings.HasSuffix(h, ".local") && !mdnsAdvertisableHost(h)
}

// mdnsConfig is the advertiser's desired configuration, derived from the
// cold settings at Start. nil = disabled.
type mdnsConfig struct {
	name   string // full advertised name, e.g. "janus.local"
	port   int    // front-door port (the _http._tcp SRV port)
	ifaces []string
	apps   bool
}

func mdnsIfacesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mdnsEntry is one live service registration.
type mdnsEntry struct {
	name      string // desired full name, e.g. "shop.local"
	app       string // owning app id; "" = the configured name
	typ       string
	port      int
	state     string // mdnsStateProbing | Announced | Renamed
	effective string // post-probe name (differs from name on rename)
	handle    dnssd.ServiceHandle
}

func mdnsEntryKey(typ, name string, port int) string {
	return typ + "/" + name + "/" + strconv.Itoa(port)
}

func (e *mdnsEntry) key() string { return mdnsEntryKey(e.typ, e.name, e.port) }

// mdnsAdvertisedEntry is the /1.0/mdns JSON shape for one entry.
type mdnsAdvertisedEntry struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Effective string `json:"effective,omitempty"` // set when renamed
	App       string `json:"app,omitempty"`
}

// mdnsAdvSnapshot is the advertiser's point-in-time state for /1.0/mdns
// and the status page.
type mdnsAdvSnapshot struct {
	entries       []mdnsAdvertisedEntry
	effectiveName string
	skipped       int
	announces     uint64
	withdraws     uint64
}

// mdnsAdvertiser owns the dnssd responder and the reconcile goroutine.
// Pooled: constructed once per process by the state holder, reconfigured
// by each config generation's Start.
type mdnsAdvertiser struct {
	logger   *zap.Logger
	registry *appRegistry

	// newResponder is the responder constructor; tests inject a fake.
	newResponder func() (dnssd.Responder, error)

	mu          sync.Mutex
	cfg         *mdnsConfig // desired; nil = disabled
	entries     map[string]*mdnsEntry
	skipped     map[string]bool // multi-label .local hosts currently registered
	responder   dnssd.Responder
	cancel      context.CancelFunc
	respondDone chan struct{}
	runIfaces   []string // the interface set the live responder was built with

	announces atomic.Uint64
	withdraws atomic.Uint64

	kickCh chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

func newMdnsAdvertiser(reg *appRegistry, logger *zap.Logger) *mdnsAdvertiser {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &mdnsAdvertiser{
		logger:       logger,
		registry:     reg,
		newResponder: func() (dnssd.Responder, error) { return dnssd.NewResponder() },
		entries:      map[string]*mdnsEntry{},
		skipped:      map[string]bool{},
		kickCh:       make(chan struct{}, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// run starts the reconcile loop (called once by the pooled state holder;
// tests drive reconcile() directly instead).
func (a *mdnsAdvertiser) run() {
	go func() {
		defer close(a.done)
		for {
			select {
			case <-a.stop:
				return
			case <-a.kickCh:
			}
			a.reconcile()
		}
	}()
}

// kickReconcile pings the reconcile loop. Non-blocking by construction:
// this is the registry's notification hook and must never sit in a
// mutation's critical path.
func (a *mdnsAdvertiser) kickReconcile() {
	select {
	case a.kickCh <- struct{}{}:
	default:
	}
}

// configure sets the desired configuration (nil disables). Enabling
// creates the responder synchronously so a 5353 socket failure is a hard
// Start error; everything else — including teardown — converges on the
// reconcile goroutine.
func (a *mdnsAdvertiser) configure(cfg *mdnsConfig) error {
	a.mu.Lock()
	if cfg != nil && a.responder == nil {
		r, err := a.newResponder()
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("janus mdns: responder socket: %w", err)
		}
		a.startResponderLocked(r, cfg.ifaces)
	}
	a.cfg = cfg
	a.mu.Unlock()
	a.kickReconcile()
	return nil
}

// startResponderLocked installs a responder and starts its Respond loop.
func (a *mdnsAdvertiser) startResponderLocked(r dnssd.Responder, ifaces []string) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.responder = r
	a.cancel = cancel
	a.respondDone = done
	a.runIfaces = append([]string{}, ifaces...)
	go func() {
		defer close(done)
		// Respond returns the context error on orderly teardown; the
		// ctx.Done path inside it sends the PTR goodbyes.
		_ = r.Respond(ctx)
	}()
}

// shutdown stops the loop and tears the live registrations down (PTR
// goodbyes; host records age out on their TTL). Called by the pooled
// holder's Destruct — the process is done with Janus entirely.
func (a *mdnsAdvertiser) shutdown() {
	a.mu.Lock()
	a.cfg = nil
	a.mu.Unlock()
	close(a.stop)
	<-a.done
	a.reconcile() // final pass: cfg nil → teardown
}

// reconcile converges the responder's registration set to the desired
// set. The only caller is the reconcile goroutine (and shutdown, after
// the loop exits), so responder I/O — including the multi-second probe
// per new name, during which the library answers no queries — is
// serialized here and never under a registry lock.
func (a *mdnsAdvertiser) reconcile() {
	// Phase 1: teardown when disabled or when the pinned interface set
	// changed (the answer set itself changed — a deliberate full flap).
	a.mu.Lock()
	cfg := a.cfg
	needTeardown := a.responder != nil &&
		(cfg == nil || !mdnsIfacesEqual(a.runIfaces, cfg.ifaces))
	if needTeardown {
		cancel, doneCh := a.cancel, a.respondDone
		n := len(a.entries)
		a.entries = map[string]*mdnsEntry{}
		a.skipped = map[string]bool{}
		a.responder = nil
		a.cancel, a.respondDone, a.runIfaces = nil, nil, nil
		a.withdraws.Add(uint64(n))
		a.mu.Unlock()
		cancel()
		<-doneCh
		a.logger.Info("janus mdns advertiser stopped; registrations withdrawn")
	} else {
		a.mu.Unlock()
	}
	if cfg == nil {
		return
	}

	// Phase 2: ensure a responder is running, then diff desired against
	// live under the lock.
	a.mu.Lock()
	if a.cfg != cfg {
		a.mu.Unlock()
		return // reconfigured mid-pass; the pending kick re-runs
	}
	if a.responder == nil {
		r, err := a.newResponder()
		if err != nil {
			a.mu.Unlock()
			a.logger.Error("janus mdns: responder socket", zap.Error(err))
			return
		}
		a.startResponderLocked(r, cfg.ifaces)
	}
	responder := a.responder
	desired := a.desiredLocked(cfg)
	var adds []*mdnsEntry
	var removes []*mdnsEntry
	for k, e := range a.entries {
		if _, ok := desired[k]; !ok {
			removes = append(removes, e)
			delete(a.entries, k)
		}
	}
	for k, tmpl := range desired {
		if _, ok := a.entries[k]; !ok {
			tmpl.state = mdnsStateProbing
			a.entries[k] = tmpl
			adds = append(adds, tmpl)
		}
	}
	a.mu.Unlock()

	// Phase 3: responder I/O, outside every lock.
	for _, e := range removes {
		if e.handle != nil {
			responder.Remove(e.handle)
		}
		a.withdraws.Add(1)
		a.logger.Info("janus mdns withdrew", zap.String("name", e.name), zap.String("app", e.app))
	}
	for _, e := range adds {
		label := strings.TrimSuffix(e.name, ".local")
		srv, err := dnssd.NewService(dnssd.Config{
			Name:          label,
			Type:          e.typ,
			Domain:        "local",
			Host:          label,
			Port:          e.port,
			Ifaces:        cfg.ifaces,
			BlockedIPNets: mdnsBlockedNets,
		})
		if err != nil {
			a.dropEntry(e)
			a.logger.Error("janus mdns service", zap.String("name", e.name), zap.Error(err))
			continue
		}
		handle, err := responder.Add(srv) // probes: expect multi-second latency
		a.mu.Lock()
		if a.entries[e.key()] != e {
			// Withdrawn or reconfigured while probing: the next pass owns it.
			a.mu.Unlock()
			if err == nil && handle != nil {
				responder.Remove(handle)
			}
			continue
		}
		if err != nil {
			delete(a.entries, e.key()) // a later kick retries
			a.mu.Unlock()
			a.logger.Error("janus mdns advertise failed", zap.String("name", e.name), zap.Error(err))
			continue
		}
		e.handle = handle
		e.effective = strings.TrimSuffix(handle.Service().Hostname(), ".")
		if e.effective == e.name {
			e.state = mdnsStateAnnounced
		} else {
			e.state = mdnsStateRenamed
			a.logger.Warn("janus mdns name conflict: announced under a renamed identity",
				zap.String("configured", e.name),
				zap.String("effective", e.effective),
			)
		}
		a.announces.Add(1)
		a.mu.Unlock()
		a.logger.Info("janus mdns announced",
			zap.String("name", e.effective),
			zap.String("app", e.app),
		)
	}
}

func (a *mdnsAdvertiser) dropEntry(e *mdnsEntry) {
	a.mu.Lock()
	if a.entries[e.key()] == e {
		delete(a.entries, e.key())
	}
	a.mu.Unlock()
}

// desiredLocked computes the desired registration set: the configured
// name ∪ (with apps on) every advertisable registered host. It also
// reconciles the skipped_hosts gauge, logging each newly skipped host
// once per registration.
func (a *mdnsAdvertiser) desiredLocked(cfg *mdnsConfig) map[string]*mdnsEntry {
	out := map[string]*mdnsEntry{}
	front := &mdnsEntry{name: cfg.name, typ: mdnsTypeFrontDoor, port: cfg.port}
	out[front.key()] = front
	if !cfg.apps || a.registry == nil {
		a.skipped = map[string]bool{}
		return out
	}
	newSkipped := map[string]bool{}
	for _, rec := range a.registry.list() {
		for _, h := range rec.Hosts {
			switch {
			case mdnsAdvertisableHost(h):
				e := &mdnsEntry{name: h, app: rec.ID, typ: mdnsTypeAppHost, port: 443}
				out[e.key()] = e
			case mdnsSkippedHost(h):
				newSkipped[h] = true
				if !a.skipped[h] {
					a.logger.Warn("janus mdns: registered host is not advertisable (multi-label .local); skipped",
						zap.String("host", h),
						zap.String("app", rec.ID),
					)
				}
			}
		}
	}
	a.skipped = newSkipped
	return out
}

// effectiveName is the configured name's post-conflict identity (the
// configured name itself until probing settles or when mdns is off).
func (a *mdnsAdvertiser) effectiveName(configured string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.app == "" && e.name == configured && e.effective != "" {
			return e.effective
		}
	}
	return configured
}

// snapshot is the advertiser's point-in-time state: configured-name
// entry first, then app entries by name.
func (a *mdnsAdvertiser) snapshot(configured string) mdnsAdvSnapshot {
	a.mu.Lock()
	entries := make([]mdnsEntry, 0, len(a.entries))
	for _, e := range a.entries {
		entries = append(entries, *e)
	}
	skipped := len(a.skipped)
	a.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if (entries[i].app == "") != (entries[j].app == "") {
			return entries[i].app == ""
		}
		return entries[i].name < entries[j].name
	})
	out := mdnsAdvSnapshot{
		effectiveName: configured,
		skipped:       skipped,
		announces:     a.announces.Load(),
		withdraws:     a.withdraws.Load(),
	}
	for _, e := range entries {
		adv := mdnsAdvertisedEntry{Name: e.name, State: e.state, App: e.app}
		if e.state == mdnsStateRenamed {
			adv.Effective = e.effective
		}
		if e.app == "" && e.name == configured && e.effective != "" {
			out.effectiveName = e.effective
		}
		out.entries = append(out.entries, adv)
	}
	return out
}

// --- App wiring: Start / Stop -------------------------------------------------

// startMdns is App.Start's mdns leg: hard checks (pinned interfaces,
// HTTP-app collision), the front-door bind, and the advertiser handoff.
// With mdns absent it converges the pooled advertiser to disabled (a
// reload that removed mdns is a real teardown).
func (a *App) startMdns() error {
	adv := a.state.mdns
	if a.Mdns == nil {
		return adv.configure(nil)
	}
	ms := a.Mdns
	for _, ifn := range ms.Interfaces {
		if _, err := net.InterfaceByName(ifn); err != nil {
			return fmt.Errorf("janus mdns: pinned interface %q does not exist on this machine: %w", ifn, err)
		}
	}
	if err := a.checkMdnsListenCollision(); err != nil {
		return err
	}

	// Bind through Caddy's listener API so the front-door socket pools
	// across config swaps (the control-listener model): an unchanged
	// address is reused, never rebound.
	na, err := caddy.ParseNetworkAddress("tcp/" + ms.Listen)
	if err != nil {
		return fmt.Errorf("janus mdns: listen address %q: %w", ms.Listen, err)
	}
	lnAny, err := na.Listen(a.ctx, 0, net.ListenConfig{})
	if err != nil {
		return fmt.Errorf("janus mdns: front door bind %s: %w", ms.Listen, err)
	}
	ln, ok := lnAny.(net.Listener)
	if !ok {
		return fmt.Errorf("janus mdns: listen %s: %T is not a stream listener", ms.Listen, lnAny)
	}
	srv := &http.Server{
		Handler:           a.mdnsFrontDoor(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a.mdnsSrv = srv
	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
			a.logger.Error("janus mdns front door stopped", zap.Error(serveErr))
		}
	}()
	a.logger.Info("janus mdns front door listening",
		zap.String("listen", ms.Listen),
		zap.String("name", ms.Name),
	)

	cfg := &mdnsConfig{
		name:   ms.Name,
		port:   ms.listenPort(),
		ifaces: append([]string{}, ms.Interfaces...),
		apps:   ms.appsOn(),
	}
	if err := adv.configure(cfg); err != nil {
		_ = a.stopMdns()
		return err
	}
	return nil
}

// stopMdns closes this config generation's front-door server; the pooled
// listener socket survives while another generation still holds it.
func (a *App) stopMdns() error {
	if a.mdnsSrv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.mdnsSrv.Shutdown(ctx)
	a.mdnsSrv = nil
	return err
}

// checkMdnsListenCollision refuses a front-door address that an HTTP-app
// server in the same config also listens on: Caddy's listener pooling
// would otherwise share the socket between two servers and split
// requests arbitrarily.
func (a *App) checkMdnsListenCollision() error {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil || httpAppI == nil {
		return nil
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return nil
	}
	for name, srv := range ha.Servers {
		for _, l := range srv.Listen {
			if mdnsListenCollides(a.Mdns.Listen, l) {
				return fmt.Errorf("janus mdns: front door %s collides with HTTP server %q listening on %s; "+
					"use `auto_https disable_redirects`, move or remove that site, or move the front door with `listen`",
					a.Mdns.Listen, name, l)
			}
		}
	}
	return nil
}

// mdnsListenCollides reports whether the front-door address and one
// HTTP-server listen address share a socket: same port (range) and
// overlapping hosts (an empty or wildcard host overlaps everything).
func mdnsListenCollides(frontDoor, serverListen string) bool {
	host, portStr, err := net.SplitHostPort(frontDoor)
	if err != nil {
		return false
	}
	port, _ := strconv.Atoi(portStr)
	na, err := caddy.ParseNetworkAddress(serverListen)
	if err != nil || na.IsUnixNetwork() {
		return false
	}
	if uint(port) < na.StartPort || uint(port) > na.EndPort {
		return false
	}
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	return wild(host) || wild(na.Host) || host == na.Host
}

// --- the front door -------------------------------------------------------------

//go:embed mdns.html
var mdnsPageHTML []byte

// mdnsFrontDoor is the front-door handler: the Host allowlist gate, then
// exactly two read-only routes. Unknown path → 404, known path with
// another method → 405 — enforced by routing, not convention.
func (a *App) mdnsFrontDoor() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.mdnsServePage)
	mux.HandleFunc("GET /status.json", a.mdnsServeStatus)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHostHeader(r.Host)
		if !a.mdnsHostAllowed(host) {
			http.Error(w, "misdirected request: this host is not served here", http.StatusMisdirectedRequest)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// mdnsHostAllowed is the front door's Host allowlist: the configured
// name, the effective (post-conflict) name, the canonical hostname when
// set, and any IP literal. Everything else — notably a DNS-rebinding
// attacker's own domain resolving here — answers 421.
func (a *App) mdnsHostAllowed(host string) bool {
	bare := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if net.ParseIP(bare) != nil {
		return true
	}
	ms := a.Mdns
	if host == ms.Name {
		return true
	}
	if ms.canonicalHost != "" && host == ms.canonicalHost {
		return true
	}
	return host == a.state.mdns.effectiveName(ms.Name)
}

func (a *App) mdnsServePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(mdnsPageHTML)
}

func (a *App) mdnsServeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.mdnsStatusSnapshot())
}

// --- the status snapshot ---------------------------------------------------------

// The snapshot is allowlist-shaped: only the fields below are emitted,
// so a future registry field stays private on this surface until someone
// deliberately adds it. Upstream socket paths and bridge_path never
// appear — counts and health only.

type mdnsStatusUpstreams struct {
	Total   int `json:"total"`
	Healthy int `json:"healthy"`
}

type mdnsStatusCache struct {
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Coalesced int64 `json:"coalesced"`
}

type mdnsStatusCacheTotals struct {
	mdnsStatusCache
	Bypass int64 `json:"bypass"`
}

type mdnsStatusHub struct {
	Conns    int `json:"conns"`
	Channels int `json:"channels"`
}

type mdnsStatusApp struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Hosts          []string            `json:"hosts"`
	Upstreams      mdnsStatusUpstreams `json:"upstreams"`
	HeartbeatAgeMS int64               `json:"heartbeat_age_ms"`
	Cache          mdnsStatusCache     `json:"cache"`
	Hub            mdnsStatusHub       `json:"hub"`
}

type mdnsStatusSnapshot struct {
	Name          string                `json:"name"`
	EffectiveName string                `json:"effective_name"`
	Canonical     string                `json:"canonical,omitempty"`
	Advertised    []string              `json:"advertised"`
	SkippedHosts  int                   `json:"skipped_hosts"`
	Apps          []mdnsStatusApp       `json:"apps"`
	Cache         mdnsStatusCacheTotals `json:"cache"`
	Hub           mdnsStatusHub         `json:"hub"`
}

// mdnsStatusSnapshot reads registry, data plane, cache, and hub state
// in-process — never a /1.0 proxy — into the redacted status shape.
func (a *App) mdnsStatusSnapshot() mdnsStatusSnapshot {
	ms := a.Mdns
	adv := a.state.mdns.snapshot(ms.Name)
	out := mdnsStatusSnapshot{
		Name:          ms.Name,
		EffectiveName: adv.effectiveName,
		Canonical:     ms.Canonical,
		Advertised:    []string{},
		SkippedHosts:  adv.skipped,
		Apps:          []mdnsStatusApp{},
	}
	for _, e := range adv.entries {
		name := e.Name
		if e.Effective != "" {
			name = e.Effective
		}
		out.Advertised = append(out.Advertised, name)
	}

	ages := a.appsReg.heartbeatAges()
	cacheStats := a.cache.snapshot()
	hubStats := a.hubs.stats()
	out.Cache = mdnsStatusCacheTotals{
		mdnsStatusCache: mdnsStatusCache{
			Hits:      cacheStats.Hits,
			Misses:    cacheStats.Misses,
			Coalesced: cacheStats.Coalesced,
		},
		Bypass: cacheStats.Bypass,
	}
	out.Hub = mdnsStatusHub{Conns: hubStats.Conns, Channels: hubStats.Channels}

	for _, rec := range a.appsReg.list() { // sorted by id
		app := mdnsStatusApp{
			ID:             rec.ID,
			Name:           rec.Name,
			Hosts:          rec.Hosts,
			HeartbeatAgeMS: ages[rec.ID].Milliseconds(),
		}
		if a.dp != nil {
			app.Upstreams.Total, app.Upstreams.Healthy = a.dp.upstreamHealth(rec.Upstreams)
		}
		if b := cacheStats.Apps[rec.ID]; b != nil {
			app.Cache = mdnsStatusCache{Hits: b.Hits, Misses: b.Misses, Coalesced: b.Coalesced}
		}
		if b := hubStats.Apps[rec.ID]; b != nil {
			app.Hub = mdnsStatusHub{Conns: b.Conns, Channels: b.Channels}
		}
		out.Apps = append(out.Apps, app)
	}
	return out
}
