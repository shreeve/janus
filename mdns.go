package janus

import (
	"context"
	_ "embed"
	"errors"
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
//
// Every string here must be a valid CIDR: the library's
// dnssd.NewService checks the wrong error variable when parsing
// BlockedIPNets (service.go), so an invalid entry is stored as a nil
// *net.IPNet and panics at answer time instead of failing at
// construction. TestMdnsBlockedNetsParse pins that all three parse.
var mdnsBlockedNets = []string{"127.0.0.0/8", "::1/128", "169.254.0.0/16"}

// Advertised-entry states — the pinned /1.0/mdns enum.
const (
	mdnsStateProbing   = "probing"
	mdnsStateAnnounced = "announced"
	mdnsStateRenamed   = "renamed"
	mdnsStateFailed    = "failed" // responder.Add failed; retried on the reconcile cadence
)

// mdnsReconcilePeriod is the reconcile loop's periodic pass: it retries
// failed Adds, rebuilds a dead responder, and observes post-announce
// conflict renames (the library renames by replacing the handle's
// service; re-reading the handle is the only signal it offers).
const mdnsReconcilePeriod = 5 * time.Second

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

	// listen and canonical do not drive the advertiser; they ride along
	// so the aborted-reload divergence log can state the full front-door
	// posture of both generations.
	listen    string
	canonical string
}

// equal reports whether two desired configurations are the same mdns
// posture (nil = disabled). An aborted reload whose mdns settings match
// the surviving generation's has no divergence to report.
func (c *mdnsConfig) equal(o *mdnsConfig) bool {
	if c == nil || o == nil {
		return c == o
	}
	return c.name == o.name && c.port == o.port && c.apps == o.apps &&
		c.listen == o.listen && c.canonical == o.canonical &&
		mdnsIfacesEqual(c.ifaces, o.ifaces)
}

// describe renders a configuration for the divergence log: every knob
// an operator would compare, on one line.
func (c *mdnsConfig) describe() string {
	if c == nil {
		return "disabled"
	}
	listen := c.listen
	if listen == "" {
		listen = fmt.Sprintf("shared(:%d)", c.port)
	}
	return fmt.Sprintf("name=%s listen=%s apps=%t interfaces=%v canonical=%q",
		c.name, listen, c.apps, c.ifaces, c.canonical)
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
	state     string // mdnsStateProbing | mdnsStateAnnounced | mdnsStateRenamed | mdnsStateFailed
	effective string // last observed on-air name (differs from name on rename)
	handle    dnssd.ServiceHandle
}

// observed derives the entry's current on-air identity from its live
// service handle. The library renames a service on conflict by
// replacing the handle's service — at Add for pre-announce conflicts,
// inside reprobe for post-announce conflicts, and inside Respond's
// startup loop for an Add that landed before the responder was running
// — so the handle is re-read at every observation instead of trusting
// a value cached at Add time. (The library writes the reprobe rename
// outside its own mutex, so this read is unsynchronized with that one
// writer: a pointer swap, observed either before or after, never torn
// in practice — the price of the only observation mechanism offered.)
// Entries without a handle (probing, failed) report their stored state.
func (e *mdnsEntry) observed() (effective, state string) {
	if e.handle == nil {
		return e.effective, e.state
	}
	eff := strings.TrimSuffix(e.handle.Service().Hostname(), ".")
	if eff == e.name {
		return eff, mdnsStateAnnounced
	}
	return eff, mdnsStateRenamed
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
	lastGen     any         // the config generation whose configure last took hold
	prevCfg     *mdnsConfig // the configuration that configure replaced
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
// tests drive reconcile() directly instead). Besides registry kicks, the
// loop runs a periodic pass so a failed Add retries, a dead responder
// rebuilds, and a post-announce conflict rename surfaces without waiting
// for the next registry mutation.
func (a *mdnsAdvertiser) run() {
	go func() {
		defer close(a.done)
		ticker := time.NewTicker(mdnsReconcilePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-a.stop:
				return
			case <-a.kickCh:
			case <-ticker.C:
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
// reconcile goroutine. gen identifies the calling config generation (the
// *App); generationRetired uses it to recognize the aborted-reload
// signature. On the error path nothing is recorded: the previous
// generation's settings stay on the air and no divergence exists.
func (a *mdnsAdvertiser) configure(gen any, cfg *mdnsConfig) error {
	a.mu.Lock()
	if cfg != nil && a.responder == nil {
		r, err := a.newResponder()
		if err != nil {
			a.mu.Unlock()
			return fmt.Errorf("janus mdns: responder socket: %w", err)
		}
		a.startResponderLocked(r, cfg.ifaces)
	}
	a.prevCfg = a.cfg
	a.cfg = cfg
	a.lastGen = gen
	a.mu.Unlock()
	a.kickReconcile()
	return nil
}

// mdnsAbortedReloadMsg is the divergence ERROR's message, pinned by
// TestMdnsAbortedReloadDetection.
const mdnsAbortedReloadMsg = "janus mdns: aborted config reload left the pooled advertiser running the aborted generation's settings; the surviving config expects the settings it was reloading away from; rollback is not performed — mdns reconciles on the next successful reload"

// generationRetired is the aborted-reload detector, called by
// App.Cleanup when a config generation releases its pooled-state
// reference while another generation survives (janusPool.Delete
// returned deleted == false). On a successful reload the retiring
// generation is the old one and the surviving new generation has
// already reconfigured the advertiser, so the identity check fails and
// nothing logs; process shutdown is the last reference and never
// reaches here; first boot retires nothing. Only an aborted reload
// retires the generation that configured last while an older one
// survives — the advertiser keeps running the aborted settings, and
// that divergence is ERROR-logged with both configurations. Rollback is
// deliberately not performed (owner ruling): state reconciles on the
// next successful reload.
func (a *mdnsAdvertiser) generationRetired(gen any) {
	a.mu.Lock()
	if a.lastGen != gen {
		a.mu.Unlock()
		return
	}
	running, expected := a.cfg, a.prevCfg
	a.mu.Unlock()
	if running.equal(expected) {
		return // identical mdns settings on both generations: no divergence
	}
	a.logger.Error(mdnsAbortedReloadMsg,
		zap.String("running", running.describe()),
		zap.String("expected", expected.describe()),
	)
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
		// Respond returns the context error on orderly teardown (its
		// ctx.Done path sends the PTR goodbyes); anything else means
		// the read loop is down — no queries are being answered — and
		// must be loud. The closed done channel is what the reconcile
		// loop's dead-responder check watches to rebuild.
		if err := r.Respond(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.logger.Error("janus mdns responder read loop failed; rebuilding on the next reconcile pass",
				zap.Error(err))
		}
	}()
}

// shutdown stops the loop and tears the live registrations down (PTR
// goodbyes; host records age out on their TTL). Called by the pooled
// holder's Destruct — the process is done with Janus entirely. Waiting
// for the in-flight reconcile pass can take up to the library's probe
// cap (60s under sustained conflict) if shutdown lands mid-probe.
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
	// Phase 1: teardown. A responder whose Respond loop died on its own
	// is discarded and rebuilt from zero — nothing was withdrawn, so the
	// withdraws counter does not move. Disabling, or changing the pinned
	// interface set (the answer set itself changed), is a deliberate
	// full teardown with goodbyes.
	a.mu.Lock()
	cfg := a.cfg
	respondDied := false
	if a.responder != nil {
		select {
		case <-a.respondDone:
			// The read loop exited without being cancelled: no queries
			// are being answered no matter what Add returns. Rebuild.
			respondDied = true
			a.cancel()
			a.responder = nil
			a.cancel, a.respondDone, a.runIfaces = nil, nil, nil
			a.entries = map[string]*mdnsEntry{}
			a.skipped = map[string]bool{}
		default:
		}
	}
	needTeardown := a.responder != nil &&
		(cfg == nil || !mdnsIfacesEqual(a.runIfaces, cfg.ifaces))
	if needTeardown {
		cancel, doneCh := a.cancel, a.respondDone
		// Only entries that reached the responder (a live handle) are
		// counted as withdrawn: the ctx-cancel goodbye covers managed
		// services only, and an entry still mid-probe was never on the
		// air — the counters count responder operations, not entries.
		n := 0
		for _, e := range a.entries {
			if e.handle != nil {
				n++
			}
		}
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
	if respondDied {
		a.logger.Error("janus mdns responder died; rebuilding and re-announcing every name")
	}
	if cfg == nil {
		return
	}

	// Phase 2: ensure a responder is running, then diff desired against
	// live under the lock. Entries whose Add failed re-enter the add set
	// — the periodic pass is the retry cadence.
	a.mu.Lock()
	if a.cfg != cfg {
		a.mu.Unlock()
		return // reconfigured mid-pass; the pending kick re-runs
	}
	if a.responder == nil {
		r, err := a.newResponder()
		if err != nil {
			a.mu.Unlock()
			// Start already returned (this is a rebuild or an interface
			// flap), so a hard error is impossible here: stay loud and
			// let the periodic pass retry.
			a.logger.Error("janus mdns: responder socket; retrying on the reconcile cadence", zap.Error(err))
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
		e, ok := a.entries[k]
		switch {
		case !ok:
			tmpl.state = mdnsStateProbing
			a.entries[k] = tmpl
			adds = append(adds, tmpl)
		case e.state == mdnsStateFailed:
			e.state = mdnsStateProbing
			adds = append(adds, e)
		}
	}
	a.mu.Unlock()

	// Phase 3: responder I/O, outside every lock.
	for _, e := range removes {
		if e.handle != nil {
			responder.Remove(e.handle)
			a.withdraws.Add(1)
			a.logger.Info("janus mdns withdrew", zap.String("name", e.name), zap.String("app", e.app))
		} else {
			// Never reached the responder (probing or failed): nothing
			// to remove, nothing to count.
			a.logger.Info("janus mdns dropped un-announced entry",
				zap.String("name", e.name), zap.String("app", e.app), zap.String("state", e.state))
		}
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
			a.markFailed(e)
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
				a.withdraws.Add(1)
			}
			continue
		}
		if err != nil {
			// The entry stays, visibly failed on /1.0/mdns, and the
			// periodic pass retries — a transient probe failure never
			// leaves a configured name silently unadvertised.
			e.state = mdnsStateFailed
			a.mu.Unlock()
			a.logger.Error("janus mdns advertise failed; retrying on the reconcile cadence",
				zap.String("name", e.name), zap.Error(err))
			continue
		}
		e.handle = handle
		e.effective, e.state = e.observed()
		if e.state == mdnsStateRenamed {
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

	// Phase 4: observe live handles. Post-announce conflicts rename a
	// service inside the library (reprobe replaces the handle's
	// service), and an Add that raced the responder's startup is probed
	// — and possibly renamed — inside Respond's startup loop; neither
	// path reports back, so the handle is the observation mechanism.
	// A newly observed rename updates the stored state and logs WARN
	// once, on this pass or the next periodic one.
	a.mu.Lock()
	type rename struct{ configured, effective string }
	var renames []rename
	for _, e := range a.entries {
		eff, state := e.observed()
		if eff == e.effective && state == e.state {
			continue
		}
		if state == mdnsStateRenamed && eff != e.effective {
			renames = append(renames, rename{e.name, eff})
		}
		e.effective, e.state = eff, state
	}
	a.mu.Unlock()
	for _, r := range renames {
		a.logger.Warn("janus mdns name conflict: announced under a renamed identity",
			zap.String("configured", r.configured),
			zap.String("effective", r.effective),
		)
	}
}

// markFailed marks an entry failed (still visible on /1.0/mdns; the
// periodic pass retries it) unless it was withdrawn mid-flight.
func (a *mdnsAdvertiser) markFailed(e *mdnsEntry) {
	a.mu.Lock()
	if a.entries[e.key()] == e {
		e.state = mdnsStateFailed
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

// carriesHost reports whether the advertiser currently carries host —
// as a desired name or as an observed on-air identity (a conflict
// rename answers to the renamed name). The shared-mode front-door
// decider reads this per request, so a hot registration is claimed the
// moment its entry lands and released the moment it withdraws.
func (a *mdnsAdvertiser) carriesHost(host string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.name == host {
			return true
		}
		if eff, _ := e.observed(); eff == host {
			return true
		}
	}
	return false
}

// effectiveName is the configured name's post-conflict identity (the
// configured name itself until probing settles or when mdns is off).
// Derived from the live handle so a post-announce rename is reflected
// immediately, not on the next reconcile pass.
func (a *mdnsAdvertiser) effectiveName(configured string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.entries {
		if e.app == "" && e.name == configured {
			if eff, _ := e.observed(); eff != "" {
				return eff
			}
		}
	}
	return configured
}

// snapshot is the advertiser's point-in-time state: configured-name
// entry first, then app entries by name. Each entry's effective name
// and state are derived from its live handle at snapshot time (the
// observed() contract), so /1.0/mdns is truthful about a post-announce
// rename even between reconcile passes.
func (a *mdnsAdvertiser) snapshot(configured string) mdnsAdvSnapshot {
	a.mu.Lock()
	entries := make([]mdnsEntry, 0, len(a.entries))
	for _, e := range a.entries {
		c := *e
		c.effective, c.state = e.observed()
		entries = append(entries, c)
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

// startMdns is App.Start's mdns leg: hard checks (pinned interfaces;
// site coverage in shared mode, HTTP-app collision in dedicated mode),
// the front door (the shared decider or the dedicated bind), and the
// advertiser handoff. With mdns absent it converges the pooled
// advertiser to disabled (a reload that removed mdns is a real
// teardown).
func (a *App) startMdns() error {
	adv := a.state.mdns
	if a.Mdns == nil {
		return adv.configure(a, nil)
	}
	ms := a.Mdns
	for _, ifn := range ms.Interfaces {
		if _, err := net.InterfaceByName(ifn); err != nil {
			return fmt.Errorf("janus mdns: pinned interface %q does not exist on this machine: %w", ifn, err)
		}
	}

	port := ms.listenPort()
	if ms.shared() {
		// Shared mode: the front door rides inside the HTTP app's
		// plain-HTTP port server; the janus site handler is the
		// per-request decider. A configuration with no janus site
		// covering the configured name on that port would leave the
		// front door silently unreachable — hard Start error instead.
		sharedPort, err := a.checkMdnsSharedCoverage()
		if err != nil {
			return err
		}
		a.enableSharedFrontDoor(sharedPort)
		port = sharedPort
		a.logger.Info("janus mdns front door shared",
			zap.Int("http_port", sharedPort),
			zap.String("name", ms.Name),
		)
	} else {
		if err := a.checkMdnsListenCollision(); err != nil {
			return err
		}

		// Bind through Caddy's listener API so the front-door socket
		// pools across config swaps (the control-listener model): an
		// unchanged address is reused, never rebound.
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
	}

	cfg := &mdnsConfig{
		name:      ms.Name,
		port:      port,
		ifaces:    append([]string{}, ms.Interfaces...),
		apps:      ms.appsOn(),
		listen:    ms.Listen,
		canonical: ms.Canonical,
	}
	if err := adv.configure(a, cfg); err != nil {
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

// mdnsSharedPasteBlock is the exact site block the shared-coverage
// Start error tells the operator to paste — the canonical shared-mode
// operator config.
const mdnsSharedPasteBlock = "http://*.local {\n\tjanus\n}"

// mdnsSharedCoverageErr is the shared-mode hard Start error: no janus
// site on the HTTP port covers the configured name, so the front door
// would be silently unreachable.
func mdnsSharedCoverageErr(httpPort int, name string) error {
	return fmt.Errorf("janus mdns: shared front door: no janus site on the HTTP port (:%d) has a host matcher covering %s; add this site block:\n\n%s\n\nor pin a dedicated front-door address with `listen`",
		httpPort, name, mdnsSharedPasteBlock)
}

// checkMdnsSharedCoverage verifies at Start (the HTTP app is provisioned
// by now — the hub site-table seam) that some janus site on the HTTP
// app's plain-HTTP port covers the configured name, and returns that
// port. Wildcard and explicit host matchers both satisfy it.
func (a *App) checkMdnsSharedCoverage() (int, error) {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil {
		if errors.Is(err, caddy.ErrNotConfigured) {
			// No HTTP app: nothing serves the shared port at all.
			return 0, mdnsSharedCoverageErr(80, a.Mdns.Name)
		}
		// The coverage gate is load-bearing; a failure to load the
		// http app must not silently disable it.
		return 0, fmt.Errorf("janus mdns: shared front-door coverage check: loading http app: %w", err)
	}
	if httpAppI == nil {
		return 0, mdnsSharedCoverageErr(80, a.Mdns.Name)
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return 0, fmt.Errorf("janus mdns: shared front-door coverage check: http app is %T, not *caddyhttp.App", httpAppI)
	}
	port := ha.HTTPPort
	if port == 0 {
		port = 80
	}
	if !mdnsSharedSiteCovers(ha, port, a.Mdns.Name) {
		return 0, mdnsSharedCoverageErr(port, a.Mdns.Name)
	}
	return port, nil
}

// mdnsSharedSiteCovers reports whether any janus site route on a server
// listening on the HTTP port has a host matcher covering name (a
// catch-all janus route covers everything).
func mdnsSharedSiteCovers(ha *caddyhttp.App, httpPort int, name string) bool {
	for _, srv := range ha.Servers {
		if !mdnsServerListensOnPort(srv, httpPort) {
			continue
		}
		var entries []hubSiteEntry
		collectHubRoutes(srv.Routes, nil, &entries)
		for _, e := range entries {
			if entryMatchesHost(e, name) {
				return true
			}
		}
	}
	return false
}

// mdnsServerListensOnPort reports whether one HTTP server binds the
// given TCP port on any of its listen addresses.
func mdnsServerListensOnPort(srv *caddyhttp.Server, port int) bool {
	for _, l := range srv.Listen {
		na, err := caddy.ParseNetworkAddress(l)
		if err != nil || na.IsUnixNetwork() {
			continue
		}
		if uint(port) >= na.StartPort && uint(port) <= na.EndPort {
			return true
		}
	}
	return false
}

// enableSharedFrontDoor wires the shared-mode decider: the HTTP port the
// janus site handlers compare the request's local port against, and the
// front-door routes they serve for front-door Hosts.
func (a *App) enableSharedFrontDoor(port int) {
	a.mdnsSharedPort = port
	a.mdnsSharedRoutes = a.mdnsRoutes()
}

// requestLocalPort is the TCP port the request's connection was
// accepted on (net/http stamps it on every served request); 0 when
// absent. The shared-mode decider compares it against the HTTP port so
// a janus site on some other plain-HTTP port stays pure data plane.
func requestLocalPort(r *http.Request) int {
	addr, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if addr == nil {
		return 0
	}
	if ta, ok := addr.(*net.TCPAddr); ok {
		return ta.Port
	}
	_, portStr, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0
	}
	port, _ := strconv.Atoi(portStr)
	return port
}

// mdnsSharedHostMine is the shared-mode decider's Host membership: the
// live front-door set is the configured name, the effective
// (post-conflict) name, every currently-carried .local app host, and
// the canonical hostname when set (the redirect loop guard serves the
// page at the canonical name too). IP literals are deliberately NOT
// mine — http://<lan-ip>/ passes through to the HTTP server's other
// routes (contract trade; dedicated mode serves IP literals).
func (a *App) mdnsSharedHostMine(host string) bool {
	ms := a.Mdns
	if host == ms.Name {
		return true
	}
	if ms.canonicalHost != "" && host == ms.canonicalHost {
		return true
	}
	return a.state.mdns.carriesHost(host)
}

// checkMdnsListenCollision refuses a dedicated front-door address that
// an HTTP-app server in the same config also listens on: Caddy's
// listener pooling would otherwise share the socket between two servers
// and split requests arbitrarily. Dedicated mode only — shared mode
// rides inside the HTTP server on purpose.
func (a *App) checkMdnsListenCollision() error {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil {
		if errors.Is(err, caddy.ErrNotConfigured) {
			return nil // no http app in this config — no server to collide with
		}
		// The collision gate is load-bearing; a failure to load the
		// http app must not silently disable it.
		return fmt.Errorf("janus mdns: front-door collision check: loading http app: %w", err)
	}
	if httpAppI == nil {
		return nil
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return fmt.Errorf("janus mdns: front-door collision check: http app is %T, not *caddyhttp.App", httpAppI)
	}
	for name, srv := range ha.Servers {
		for _, l := range srv.Listen {
			if mdnsListenCollides(a.Mdns.Listen, l) {
				return fmt.Errorf("janus mdns: front door %s collides with HTTP server %q listening on %s; "+
					"drop `listen` to serve the front door inside that server (shared mode), "+
					"move the front door with `listen`, or move or remove that site",
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
	return mdnsListenHostsOverlap(host, na.Host)
}

// mdnsListenHostsOverlap reports whether two listen hosts can bind the
// same address. Wildcards overlap everything; otherwise a hostname
// listen (e.g. "localhost" vs "127.0.0.1") hides address equality from
// a string compare, so both sides resolve to address sets and overlap
// on any shared address. This runs at Start, where the contract's
// promise is the precise named-server error, not a raw OS bind failure.
func mdnsListenHostsOverlap(a, b string) bool {
	wild := func(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }
	if wild(a) || wild(b) {
		return true
	}
	if a == b {
		return true
	}
	for _, x := range mdnsListenHostAddrs(a) {
		for _, y := range mdnsListenHostAddrs(b) {
			if x == y {
				return true
			}
		}
	}
	return false
}

// mdnsListenHostAddrs normalizes one listen host to its address set: an
// IP literal is itself (canonicalized), a hostname resolves. A hostname
// that does not resolve falls back to the literal — the bind that
// follows fails loudly on its own.
func mdnsListenHostAddrs(h string) []string {
	if ip := net.ParseIP(h); ip != nil {
		return []string{ip.String()}
	}
	addrs, err := net.LookupHost(h)
	if err != nil {
		return []string{h}
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if ip := net.ParseIP(addr); ip != nil {
			out = append(out, ip.String())
			continue
		}
		out = append(out, addr)
	}
	return out
}

// --- the front door -------------------------------------------------------------

//go:embed mdns.html
var mdnsPageHTML []byte

// mdnsRoutes is the front door's route set, identical in both modes:
// exactly two read-only routes. Unknown path → 404, known path with
// another method → 405 — enforced by routing, not convention.
func (a *App) mdnsRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.mdnsServePage)
	mux.HandleFunc("GET /status.json", a.mdnsServeStatus)
	return mux
}

// mdnsFrontDoor is the dedicated-mode front-door handler: the Host
// allowlist gate (421 for everything not mine — the listener serves
// nothing else), then the routes. Shared mode never uses it: there the
// janus site handler is the decider and not-mine passes through to the
// HTTP server's other routes instead of 421.
func (a *App) mdnsFrontDoor() http.Handler {
	routes := a.mdnsRoutes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHostHeader(r.Host)
		if !a.mdnsHostAllowed(host) {
			http.Error(w, "misdirected request: this host is not served here", http.StatusMisdirectedRequest)
			return
		}
		routes.ServeHTTP(w, r)
	})
}

// mdnsHostAllowed is the dedicated front door's Host allowlist: the
// configured name, the effective (post-conflict) name, the canonical
// hostname when set, and any IP literal. Everything else — notably a
// DNS-rebinding attacker's own domain resolving here — answers 421.
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
