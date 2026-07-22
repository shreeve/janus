package janus

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/dnssd"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// --- parse ---------------------------------------------------------------------

func TestParseMdnsDirectiveLegal(t *testing.T) {
	cases := []struct {
		name string
		cf   string
		want func(*MdnsSettings) bool
	}{
		{"bare", "janus {\n mdns \n}", func(ms *MdnsSettings) bool { return ms != nil }},
		{"empty block", "janus {\n mdns {\n} \n}", func(ms *MdnsSettings) bool { return ms != nil }},
		{"name lowercased", "janus {\n mdns {\n name JANUS.Local \n} \n}",
			func(ms *MdnsSettings) bool { return ms.Name == "janus.local" }},
		{"canonical with port", "janus {\n mdns {\n canonical https://janus.lan.ripdev.io:8443 \n} \n}",
			func(ms *MdnsSettings) bool { return ms.Canonical == "https://janus.lan.ripdev.io:8443" }},
		{"canonical trailing slash normalized", "janus {\n mdns {\n canonical https://janus.lan.ripdev.io/ \n} \n}",
			func(ms *MdnsSettings) bool { return ms.Canonical == "https://janus.lan.ripdev.io" }},
		{"interfaces", "janus {\n mdns {\n interface en0 en1 \n} \n}",
			func(ms *MdnsSettings) bool { return len(ms.Interfaces) == 2 }},
		{"apps off", "janus {\n mdns {\n apps off \n} \n}",
			func(ms *MdnsSettings) bool { return ms.Apps != nil && !*ms.Apps }},
		{"listen", "janus {\n mdns {\n listen :7680 \n} \n}",
			func(ms *MdnsSettings) bool { return ms.Listen == ":7680" }},
		{"everything", "janus {\n mdns {\n name edge.local\n canonical https://janus.lan.ripdev.io\n interface en0\n apps on\n listen :7680 \n} \n}",
			func(ms *MdnsSettings) bool {
				return ms.Name == "edge.local" && ms.Canonical == "https://janus.lan.ripdev.io" &&
					len(ms.Interfaces) == 1 && ms.Apps != nil && *ms.Apps && ms.Listen == ":7680"
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tc.cf)
			app := new(App)
			if err := app.UnmarshalCaddyfile(d); err != nil {
				t.Fatal(err)
			}
			if !tc.want(app.Mdns) {
				t.Fatalf("parse of %q: %+v", tc.cf, app.Mdns)
			}
		})
	}
}

func TestParseMdnsDirectiveHardErrors(t *testing.T) {
	globalCases := []string{
		`janus { mdns on }`,
		`janus { mdns off }`,
		`janus { mdns yes }`,
		`janus { mdns } janus { mdns }`,
		`janus { mdns mdns }`,
		`janus { mdns { bogus 1 } }`,
		`janus { mdns { name } }`,
		`janus { mdns { name janus } }`,
		`janus { mdns { name a.b.local } }`,
		`janus { mdns { name janus.lan } }`,
		`janus { mdns { name janus.local. } }`,
		`janus { mdns { name -janus.local } }`,
		`janus { mdns { name janus-.local } }`,
		`janus { mdns { name jänus.local } }`,
		`janus { mdns { name janus.local extra } }`,
		`janus { mdns { canonical } }`,
		`janus { mdns { canonical http://x.example.com } }`,
		`janus { mdns { canonical https://x.example.com/path } }`,
		`janus { mdns { canonical https://x.example.com?q=1 } }`,
		`janus { mdns { canonical "https://x.example.com#frag" } }`,
		`janus { mdns { canonical https://user@x.example.com } }`,
		`janus { mdns { canonical https://192.168.1.10 } }`,
		`janus { mdns { canonical "https://[fe80::1]" } }`,
		`janus { mdns { canonical https://x.local } }`,
		`janus { mdns { canonical "https://not a host" } }`,
		`janus { mdns { interface } }`,
		`janus { mdns { interface en0 en0 } }`,
		`janus { mdns { interface "" } }`,
		`janus { mdns { apps } }`,
		`janus { mdns { apps maybe } }`,
		`janus { mdns { apps on off } }`,
		`janus { mdns { listen } }`,
		`janus { mdns { listen 80 } }`,
		`janus { mdns { listen :0 } }`,
		`janus { mdns { listen :99999 } }`,
		`janus { mdns { listen :http } }`,
		`janus { mdns { name janus.local name other.local } }`,
		`janus { mdns { name janus.local { nested } } }`,
	}
	for _, cf := range globalCases {
		d := caddyfile.NewTestDispenser(cf)
		if err := new(App).UnmarshalCaddyfile(d); err == nil {
			t.Errorf("global parse accepted %q", cf)
		}
	}
	siteCases := []string{
		`janus { mdns }`,
		`janus { mdns { name x.local } }`,
	}
	for _, cf := range siteCases {
		d := caddyfile.NewTestDispenser(cf)
		var h Handler
		if err := h.UnmarshalCaddyfile(d); err == nil {
			t.Errorf("site parse accepted %q", cf)
		}
	}
}

func TestValidateMdnsNameEdges(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	label64 := strings.Repeat("a", 64)
	if _, err := validateMdnsName(label63 + ".local"); err != nil {
		t.Errorf("63-byte label rejected: %v", err)
	}
	if _, err := validateMdnsName(label64 + ".local"); err == nil {
		t.Error("64-byte label accepted")
	}
	if got, err := validateMdnsName("My-Box.LOCAL"); err != nil || got != "my-box.local" {
		t.Errorf("case-folding: got %q, %v", got, err)
	}
	for _, bad := range []string{"janus", "a.b.local", "janus.lan", "janus.local.", "local",
		".local", "-x.local", "x-.local", "jänus.local"} {
		if _, err := validateMdnsName(bad); err == nil {
			t.Errorf("accepted illegal name %q", bad)
		}
	}
}

func TestValidateMdnsCanonicalEdges(t *testing.T) {
	if got, err := validateMdnsCanonical("https://x.Example.com:8443/"); err != nil || got != "https://x.example.com:8443" {
		t.Errorf("normalization: got %q, %v", got, err)
	}
	for _, bad := range []string{
		"http://x.example.com", "https://x.example.com/page", "https://x.example.com?q=1",
		"https://x.example.com#f", "https://u:p@x.example.com", "https://10.0.0.1",
		"https://[::1]", "https://x.local", "https://", "https://x.example.com:0",
	} {
		if _, err := validateMdnsCanonical(bad); err == nil {
			t.Errorf("accepted illegal canonical %q", bad)
		}
	}
}

func TestProvisionMdnsDefaultsAndJSONPath(t *testing.T) {
	app := &App{Mdns: &MdnsSettings{}}
	if err := app.provisionMdns(); err != nil {
		t.Fatal(err)
	}
	// listen unset is the default — shared mode, the front door inside
	// the HTTP app's plain-HTTP port server.
	if app.Mdns.Name != "janus.local" || !app.Mdns.shared() || !app.Mdns.appsOn() {
		t.Fatalf("defaults: %+v", app.Mdns)
	}
	// listen set is dedicated mode with the port derived from it.
	app = &App{Mdns: &MdnsSettings{Listen: ":7680"}}
	if err := app.provisionMdns(); err != nil {
		t.Fatal(err)
	}
	if app.Mdns.shared() || app.Mdns.listenPort() != 7680 {
		t.Fatalf("dedicated: shared=%v port=%d", app.Mdns.shared(), app.Mdns.listenPort())
	}
	// The native JSON path gets the same rejections as the Caddyfile.
	for _, bad := range []*MdnsSettings{
		{Name: "a.b.local"},
		{Canonical: "http://x.example.com"},
		{Interfaces: []string{"en0", "en0"}},
		{Listen: "80"},
	} {
		app := &App{Mdns: bad}
		if err := app.provisionMdns(); err == nil {
			t.Errorf("provision accepted %+v", bad)
		}
	}
	// Canonical host derived for the allowlist.
	app = &App{Mdns: &MdnsSettings{Canonical: "https://janus.lan.ripdev.io:8443"}}
	if err := app.provisionMdns(); err != nil {
		t.Fatal(err)
	}
	if app.Mdns.canonicalHost != "janus.lan.ripdev.io" {
		t.Fatalf("canonicalHost: %q", app.Mdns.canonicalHost)
	}
}

// --- classifier and desired set --------------------------------------------------

func TestMdnsAdvertisableClassifier(t *testing.T) {
	cases := map[string]bool{
		"x.local":        true,
		"shop.local":     true,
		"x.y.local":      false,
		"x.ripdev.io":    false,
		"local":          false,
		"a.b.c.local":    false,
		"deep.dev.local": false,
	}
	for host, want := range cases {
		if got := mdnsAdvertisableHost(host); got != want {
			t.Errorf("advertisable(%q) = %v, want %v", host, got, want)
		}
	}
	skipped := map[string]bool{
		"x.y.local":   true,
		"x.local":     false,
		"x.ripdev.io": false,
		"local":       false,
	}
	for host, want := range skipped {
		if got := mdnsSkippedHost(host); got != want {
			t.Errorf("skipped(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestMdnsDesiredSet(t *testing.T) {
	reg := newAppRegistry()
	adv := newMdnsAdvertiser(reg, nil)
	rec, err := reg.create("shop", []string{"shop.local", "shop.ripdev.io", "a.b.local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &mdnsConfig{name: "janus.local", port: 80, apps: true}
	adv.mu.Lock()
	desired := adv.desiredLocked(cfg)
	adv.mu.Unlock()
	if len(desired) != 2 {
		t.Fatalf("desired = %d entries, want 2 (front + shop.local)", len(desired))
	}
	if _, ok := desired[mdnsEntryKey(mdnsTypeAppHost, "shop.local", 443)]; !ok {
		t.Fatal("shop.local missing from desired set")
	}
	if len(adv.skipped) != 1 || !adv.skipped["a.b.local"] {
		t.Fatalf("skipped gauge set: %v", adv.skipped)
	}

	// apps off: only the configured name; suppression is not skipping.
	cfgOff := &mdnsConfig{name: "janus.local", port: 80, apps: false}
	adv.mu.Lock()
	desired = adv.desiredLocked(cfgOff)
	adv.mu.Unlock()
	if len(desired) != 1 || len(adv.skipped) != 0 {
		t.Fatalf("apps off: %d entries, %d skipped", len(desired), len(adv.skipped))
	}

	// Coexistence: an app host equal to the configured name yields two
	// registrations (never one) — different service types, one hostname.
	if _, err := reg.patch(rec.ID, nil, &[]string{"janus.local"}, nil); err != nil {
		t.Fatal(err)
	}
	adv.mu.Lock()
	desired = adv.desiredLocked(cfg)
	adv.mu.Unlock()
	if len(desired) != 2 {
		t.Fatalf("coexistence: %d entries, want 2", len(desired))
	}
	if _, ok := desired[mdnsEntryKey(mdnsTypeFrontDoor, "janus.local", 80)]; !ok {
		t.Fatal("front-door registration missing")
	}
	if _, ok := desired[mdnsEntryKey(mdnsTypeAppHost, "janus.local", 443)]; !ok {
		t.Fatal("app registration for the shared name missing")
	}
}

// --- fake responder ---------------------------------------------------------------

type fakeHandle struct {
	mu  sync.Mutex
	srv dnssd.Service
}

func (h *fakeHandle) UpdateText(map[string]string, dnssd.Responder) {}

func (h *fakeHandle) Service() dnssd.Service {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.srv
}

// rename models the library's post-announce conflict path: reprobe
// replaces the handle's service with a renamed one, and nothing reports
// back — the handle is the only observable.
func (h *fakeHandle) rename(host string) {
	h.mu.Lock()
	h.srv.Host = host
	h.mu.Unlock()
}

type fakeResponder struct {
	mu       sync.Mutex
	adds     []string
	removes  []string
	handles  map[string]*fakeHandle // original host label → live handle
	renameTo map[string]string      // desired host label → renamed label
	addErrs  map[string]int         // host label → remaining Add failures
	addDelay time.Duration
	die      chan struct{} // closed → Respond returns a non-cancel error
}

func (f *fakeResponder) Add(srv dnssd.Service) (dnssd.ServiceHandle, error) {
	if f.addDelay > 0 {
		time.Sleep(f.addDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := f.addErrs[srv.Host]; n > 0 {
		f.addErrs[srv.Host] = n - 1
		return nil, errors.New("probe socket failed")
	}
	f.adds = append(f.adds, srv.Host+"."+srv.Domain)
	orig := srv.Host
	if to, ok := f.renameTo[srv.Host]; ok {
		srv.Host = to
	}
	h := &fakeHandle{srv: srv}
	if f.handles == nil {
		f.handles = map[string]*fakeHandle{}
	}
	f.handles[orig] = h
	return h, nil
}

func (f *fakeResponder) Remove(h dnssd.ServiceHandle) {
	srv := h.Service()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes = append(f.removes, srv.Host+"."+srv.Domain)
}

func (f *fakeResponder) Respond(ctx context.Context) error {
	if f.die != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.die:
			return errors.New("responder read loop failed")
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeResponder) Debug(ctx context.Context, fn dnssd.ReadFunc) {}

func (f *fakeResponder) counts() (adds, removes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.adds), len(f.removes)
}

func newTestAdvertiser(t *testing.T, reg *appRegistry, fake *fakeResponder) *mdnsAdvertiser {
	t.Helper()
	adv := newMdnsAdvertiser(reg, nil)
	adv.newResponder = func() (dnssd.Responder, error) { return fake, nil }
	return adv
}

// --- reconcile transitions ----------------------------------------------------------

func TestMdnsReconcileTransitions(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	cfg := &mdnsConfig{name: "janus.local", port: 80, apps: true}
	if err := adv.configure(t, cfg); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap := adv.snapshot("janus.local")
	if len(snap.entries) != 1 || snap.entries[0].Name != "janus.local" || snap.entries[0].State != mdnsStateAnnounced {
		t.Fatalf("enable: %+v", snap.entries)
	}
	if snap.announces != 1 || snap.withdraws != 0 {
		t.Fatalf("enable counters: %d/%d", snap.announces, snap.withdraws)
	}

	// Create advertises.
	rec, err := reg.create("shop", []string{"shop.local", "shop.ripdev.io"}, "")
	if err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("janus.local")
	if len(snap.entries) != 2 || snap.entries[1].Name != "shop.local" || snap.entries[1].App != rec.ID {
		t.Fatalf("create: %+v", snap.entries)
	}

	// No-diff pass: zero responder calls (the no-flap pin).
	a0, r0 := fake.counts()
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if a1, r1 := fake.counts(); a1 != a0 || r1 != r0 {
		t.Fatalf("no-diff reload moved the responder: adds %d→%d removes %d→%d", a0, a1, r0, r1)
	}

	// PATCH swaps: exactly the diff.
	if _, err := reg.patch(rec.ID, nil, &[]string{"store.local"}, nil); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("janus.local")
	if len(snap.entries) != 2 || snap.entries[1].Name != "store.local" {
		t.Fatalf("patch: %+v", snap.entries)
	}
	if snap.withdraws != 1 {
		t.Fatalf("patch withdraws: %d", snap.withdraws)
	}

	// DELETE withdraws.
	if err := reg.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("janus.local")
	if len(snap.entries) != 1 {
		t.Fatalf("delete: %+v", snap.entries)
	}
	if snap.withdraws != 2 {
		t.Fatalf("delete withdraws: %d", snap.withdraws)
	}

	// Name change: old front entry withdraws, new probes.
	if err := adv.configure(t, &mdnsConfig{name: "edge.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("edge.local")
	if len(snap.entries) != 1 || snap.entries[0].Name != "edge.local" {
		t.Fatalf("rename config: %+v", snap.entries)
	}

	// Disable: teardown clears everything and counts the withdrawals.
	w0 := snap.withdraws
	if err := adv.configure(t, nil); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("edge.local")
	if len(snap.entries) != 0 {
		t.Fatalf("disable: %+v", snap.entries)
	}
	if snap.withdraws != w0+1 {
		t.Fatalf("disable withdraws: %d, want %d", snap.withdraws, w0+1)
	}
}

func TestMdnsConflictRenameSurfaces(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{renameTo: map[string]string{"janus": "janus-2"}}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap := adv.snapshot("janus.local")
	if len(snap.entries) != 1 || snap.entries[0].State != mdnsStateRenamed {
		t.Fatalf("rename state: %+v", snap.entries)
	}
	if snap.entries[0].Effective != "janus-2.local" || snap.effectiveName != "janus-2.local" {
		t.Fatalf("effective name: %+v / %q", snap.entries[0], snap.effectiveName)
	}
	if adv.effectiveName("janus.local") != "janus-2.local" {
		t.Fatalf("effectiveName accessor: %q", adv.effectiveName("janus.local"))
	}
}

// TestMdnsPostAnnounceRenameSurfaces pins the blocker fix: a conflict
// rename that happens AFTER announcement (the library's reprobe path
// replaces the handle's service and reports nothing back) surfaces on
// /1.0/mdns — at snapshot time immediately, and in the stored state on
// the next reconcile pass — never silently.
func TestMdnsPostAnnounceRenameSurfaces(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if snap := adv.snapshot("janus.local"); snap.entries[0].State != mdnsStateAnnounced {
		t.Fatalf("pre-rename: %+v", snap.entries)
	}

	// The LAN develops a conflict after announcement: the library
	// reprobes and renames inside the handle.
	fake.mu.Lock()
	h := fake.handles["janus"]
	fake.mu.Unlock()
	h.rename("janus-2")

	// Snapshot-time derivation: truthful before any reconcile pass runs.
	snap := adv.snapshot("janus.local")
	if snap.entries[0].State != mdnsStateRenamed || snap.entries[0].Effective != "janus-2.local" {
		t.Fatalf("post-rename snapshot: %+v", snap.entries)
	}
	if snap.effectiveName != "janus-2.local" {
		t.Fatalf("post-rename effectiveName: %q", snap.effectiveName)
	}
	if adv.effectiveName("janus.local") != "janus-2.local" {
		t.Fatalf("effectiveName accessor: %q", adv.effectiveName("janus.local"))
	}

	// The periodic pass folds the observation into the stored state
	// (and logs the WARN) without moving the responder.
	a0, r0 := fake.counts()
	adv.reconcile()
	if a1, r1 := fake.counts(); a1 != a0 || r1 != r0 {
		t.Fatalf("observation pass moved the responder: adds %d→%d removes %d→%d", a0, a1, r0, r1)
	}
	adv.mu.Lock()
	e := adv.entries[mdnsEntryKey(mdnsTypeFrontDoor, "janus.local", 80)]
	state, eff := e.state, e.effective
	adv.mu.Unlock()
	if state != mdnsStateRenamed || eff != "janus-2.local" {
		t.Fatalf("stored state after observation: %s / %s", state, eff)
	}
}

// TestMdnsFailedAddRetries pins the must-fix: a failed responder.Add
// leaves the entry visible on /1.0/mdns as "failed" (never silently
// absent) and the next reconcile pass — the periodic cadence in
// production — retries it to convergence.
func TestMdnsFailedAddRetries(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{addErrs: map[string]int{"janus": 1}}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap := adv.snapshot("janus.local")
	if len(snap.entries) != 1 || snap.entries[0].State != mdnsStateFailed {
		t.Fatalf("after failed Add: %+v", snap.entries)
	}
	if snap.announces != 0 || snap.withdraws != 0 {
		t.Fatalf("failed Add moved counters: %d/%d", snap.announces, snap.withdraws)
	}
	adv.reconcile() // the retry pass
	snap = adv.snapshot("janus.local")
	if len(snap.entries) != 1 || snap.entries[0].State != mdnsStateAnnounced {
		t.Fatalf("after retry: %+v", snap.entries)
	}
	if snap.announces != 1 || snap.withdraws != 0 {
		t.Fatalf("retry counters: %d/%d", snap.announces, snap.withdraws)
	}
}

// TestMdnsDeadResponderRebuilds pins the Respond-error fix: a responder
// whose read loop dies with a real error is detected on the reconcile
// cadence, discarded, and rebuilt — every name re-announces on the new
// responder, and the withdraws counter never moves (nothing was removed
// cleanly).
func TestMdnsDeadResponderRebuilds(t *testing.T) {
	reg := newAppRegistry()
	die := make(chan struct{})
	first := &fakeResponder{die: die}
	second := &fakeResponder{}
	built := 0
	adv := newMdnsAdvertiser(reg, nil)
	adv.newResponder = func() (dnssd.Responder, error) {
		built++
		if built == 1 {
			return first, nil
		}
		return second, nil
	}
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if snap := adv.snapshot("janus.local"); snap.entries[0].State != mdnsStateAnnounced {
		t.Fatalf("pre-death: %+v", snap.entries)
	}

	// The read loop dies; wait for the Respond goroutine to exit so the
	// dead-responder check observes the closed done channel.
	close(die)
	adv.mu.Lock()
	done := adv.respondDone
	adv.mu.Unlock()
	<-done

	adv.reconcile()
	if built != 2 {
		t.Fatalf("responder never rebuilt: %d constructions", built)
	}
	snap := adv.snapshot("janus.local")
	if len(snap.entries) != 1 || snap.entries[0].State != mdnsStateAnnounced {
		t.Fatalf("post-rebuild: %+v", snap.entries)
	}
	if a, _ := second.counts(); a != 1 {
		t.Fatalf("name never re-announced on the new responder: %d adds", a)
	}
	if snap.withdraws != 0 {
		t.Fatalf("death rebuild counted withdraws: %d", snap.withdraws)
	}
}

// TestMdnsTeardownCountsOnlyAnnounced pins the counter semantics: the
// disable teardown counts a withdraw only for entries that actually
// reached the responder — an entry sitting in "failed" was never on the
// air and never counts.
func TestMdnsTeardownCountsOnlyAnnounced(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{addErrs: map[string]int{"shop": 1000}}
	adv := newTestAdvertiser(t, reg, fake)
	if _, err := reg.create("shop", []string{"shop.local"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap := adv.snapshot("janus.local")
	if len(snap.entries) != 2 {
		t.Fatalf("setup: %+v", snap.entries)
	}
	if err := adv.configure(t, nil); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("janus.local")
	if snap.withdraws != 1 {
		t.Fatalf("teardown withdraws = %d, want 1 (announced front door only)", snap.withdraws)
	}
}

// TestMdnsBlockedNetsParse pins the block list against a library trap:
// dnssd.NewService checks the wrong error variable when parsing
// BlockedIPNets, so an invalid CIDR would be stored as nil and panic at
// answer time. All three entries must parse.
func TestMdnsBlockedNetsParse(t *testing.T) {
	srv, err := dnssd.NewService(dnssd.Config{
		Name:          "janus",
		Type:          mdnsTypeFrontDoor,
		Domain:        "local",
		Host:          "janus",
		Port:          80,
		BlockedIPNets: mdnsBlockedNets,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Blocked) != len(mdnsBlockedNets) {
		t.Fatalf("blocked nets: %d, want %d", len(srv.Blocked), len(mdnsBlockedNets))
	}
	for i, n := range srv.Blocked {
		if n == nil {
			t.Fatalf("blocked net %d (%q) parsed to nil", i, mdnsBlockedNets[i])
		}
	}
}

func TestMdnsSkippedGaugeLifecycle(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	rec, err := reg.create("multi", []string{"a.b.local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if snap := adv.snapshot("janus.local"); snap.skipped != 1 {
		t.Fatalf("gauge after multi-label registration: %d", snap.skipped)
	}
	if err := reg.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if snap := adv.snapshot("janus.local"); snap.skipped != 0 {
		t.Fatalf("gauge after delete: %d", snap.skipped)
	}
}

// TestMdnsNotifyNeverBlocks pins the asynchrony contract: registry
// mutations ping the reconcile loop and return immediately, even while a
// slow probe is in flight on the advertiser's goroutine.
func TestMdnsNotifyNeverBlocks(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{addDelay: 300 * time.Millisecond}
	adv := newTestAdvertiser(t, reg, fake)
	reg.mdnsNotify = adv.kickReconcile
	adv.run()
	defer adv.shutdown()
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 5; i++ {
		host := string(rune('a'+i)) + "pp.local"
		if _, err := reg.create("app"+string(rune('a'+i)), []string{host}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("registry mutations blocked on the advertiser: %v", elapsed)
	}
	// Convergence: all five names (plus the front door) eventually land.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := adv.snapshot("janus.local")
		if len(snap.entries) == 6 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never converged: %+v", snap.entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- aborted-reload detection -----------------------------------------------------

// TestMdnsAbortedReloadDetection pins the owner ruling on the
// failed-reload flag: an aborted config reload leaves the pooled
// advertiser running the aborted generation's settings (rollback is
// deliberately not performed), and the aborted generation's teardown —
// it is still the advertiser's most recent configurer while an older
// generation survives — ERROR-logs the divergence with both
// configurations. The normal paths (successful reload handover, process
// shutdown, first boot, and an abort whose mdns settings match the
// survivor's) never log it.
func TestMdnsAbortedReloadDetection(t *testing.T) {
	cfgA := &mdnsConfig{name: "janus.local", port: 80, apps: true, listen: ":80"}
	cfgB := &mdnsConfig{name: "edge.local", port: 7680, apps: false, listen: ":7680",
		canonical: "https://janus.lan.ripdev.io"}
	newObserved := func() (*mdnsAdvertiser, *observer.ObservedLogs) {
		core, logs := observer.New(zapcore.DebugLevel)
		adv := newMdnsAdvertiser(newAppRegistry(), zap.New(core))
		adv.newResponder = func() (dnssd.Responder, error) { return &fakeResponder{}, nil }
		return adv, logs
	}
	divergence := func(logs *observer.ObservedLogs) []observer.LoggedEntry {
		return logs.FilterMessage(mdnsAbortedReloadMsg).All()
	}
	genA, genB := "generation-A", "generation-B"

	t.Run("aborted reload fires", func(t *testing.T) {
		adv, logs := newObserved()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		if err := adv.configure(genB, cfgB); err != nil {
			t.Fatal(err)
		}
		adv.generationRetired(genB) // B torn down while A survives
		entries := divergence(logs)
		if len(entries) != 1 {
			t.Fatalf("divergence logs = %d, want 1", len(entries))
		}
		e := entries[0]
		if e.Level != zapcore.ErrorLevel {
			t.Fatalf("level = %v, want ERROR", e.Level)
		}
		fields := e.ContextMap()
		running, _ := fields["running"].(string)
		expected, _ := fields["expected"].(string)
		for _, want := range []string{"name=edge.local", "listen=:7680", "apps=false",
			`canonical="https://janus.lan.ripdev.io"`} {
			if !strings.Contains(running, want) {
				t.Errorf("running %q missing %q", running, want)
			}
		}
		for _, want := range []string{"name=janus.local", "listen=:80", "apps=true"} {
			if !strings.Contains(expected, want) {
				t.Errorf("expected %q missing %q", expected, want)
			}
		}
	})

	t.Run("aborted reload that disabled mdns fires", func(t *testing.T) {
		adv, logs := newObserved()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		if err := adv.configure(genB, nil); err != nil {
			t.Fatal(err)
		}
		adv.generationRetired(genB)
		entries := divergence(logs)
		if len(entries) != 1 {
			t.Fatalf("divergence logs = %d, want 1", len(entries))
		}
		if running, _ := entries[0].ContextMap()["running"].(string); running != "disabled" {
			t.Fatalf("running = %q, want %q", running, "disabled")
		}
	})

	t.Run("successful handover stays silent", func(t *testing.T) {
		adv, logs := newObserved()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		if err := adv.configure(genB, cfgB); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		adv.generationRetired(genA) // the old generation retires; B configured last
		if n := len(divergence(logs)); n != 0 {
			t.Fatalf("handover logged %d divergences", n)
		}
	})

	t.Run("process shutdown stays silent", func(t *testing.T) {
		adv, logs := newObserved()
		adv.run()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.shutdown() // last reference: generationRetired is never invoked
		if n := len(divergence(logs)); n != 0 {
			t.Fatalf("shutdown logged %d divergences", n)
		}
	})

	t.Run("first boot stays silent", func(t *testing.T) {
		adv, logs := newObserved()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		if n := len(divergence(logs)); n != 0 {
			t.Fatalf("first boot logged %d divergences", n)
		}
	})

	t.Run("identical-settings abort stays silent", func(t *testing.T) {
		adv, logs := newObserved()
		if err := adv.configure(genA, cfgA); err != nil {
			t.Fatal(err)
		}
		adv.reconcile()
		same := &mdnsConfig{name: "janus.local", port: 80, apps: true, listen: ":80"}
		if err := adv.configure(genB, same); err != nil {
			t.Fatal(err)
		}
		adv.generationRetired(genB) // nothing diverged; a false ERROR would be its own bug
		if n := len(divergence(logs)); n != 0 {
			t.Fatalf("identical-settings abort logged %d divergences", n)
		}
	})
}

// TestMdnsAbortedReloadDetectionThroughCleanup pins the seam itself:
// App.Cleanup invokes the detector only when the pool release leaves a
// surviving generation (janusPool.Delete returns deleted == false); the
// last release destructs the pooled state with no divergence log.
func TestMdnsAbortedReloadDetectionThroughCleanup(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	load := func() *janusState {
		stI, _, err := janusPool.LoadOrNew(janusStateKey, func() (caddy.Destructor, error) {
			return newJanusState(logger, time.Hour)
		})
		if err != nil {
			t.Fatal(err)
		}
		return stI.(*janusState)
	}

	st := load() // generation A provisions
	st.mdns.newResponder = func() (dnssd.Responder, error) { return &fakeResponder{}, nil }
	appA := &App{state: st}
	if err := st.mdns.configure(appA, &mdnsConfig{name: "janus.local", port: 80, listen: ":80"}); err != nil {
		t.Fatal(err)
	}

	if load() != st { // generation B provisions: same holder, second reference
		t.Fatal("pool returned a different holder")
	}
	appB := &App{state: st}
	if err := st.mdns.configure(appB, &mdnsConfig{name: "edge.local", port: 80, listen: ":80"}); err != nil {
		t.Fatal(err)
	}

	// The reload aborts: B releases its reference while A survives.
	if err := appB.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if n := len(logs.FilterMessage(mdnsAbortedReloadMsg).All()); n != 1 {
		t.Fatalf("aborted-generation cleanup: %d divergence logs, want 1", n)
	}

	// Process shutdown: the last reference destructs, no new divergence.
	if err := appA.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if n := len(logs.FilterMessage(mdnsAbortedReloadMsg).All()); n != 1 {
		t.Fatalf("final cleanup: %d divergence logs, want %d", n, 1)
	}
}

// --- front door -----------------------------------------------------------------

func newTestMdnsApp(t *testing.T, ms *MdnsSettings) *App {
	t.Helper()
	st, err := newJanusState(nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Destruct() })
	app := &App{Mdns: ms}
	if err := app.provisionMdns(); err != nil {
		t.Fatal(err)
	}
	app.state = st
	app.appsReg = st.registry
	app.dp = st.dp
	app.hubs = st.hubs
	if err := app.provisionCacheStore(); err != nil {
		t.Fatal(err)
	}
	return app
}

// TestMdnsFrontDoorRouting pins the dedicated-mode front door (listen
// set): its own listener, the strict Host allowlist with 421 for
// everything not mine, and the 404/405 route discipline.
func TestMdnsFrontDoorRouting(t *testing.T) {
	app := newTestMdnsApp(t, &MdnsSettings{Canonical: "https://janus.lan.ripdev.io", Listen: ":7680"})
	h := app.mdnsFrontDoor()
	cases := []struct {
		method, path, host string
		want               int
	}{
		{"GET", "/", "janus.local", 200},
		{"HEAD", "/", "janus.local", 200},
		{"GET", "/status.json", "janus.local", 200},
		{"HEAD", "/status.json", "janus.local", 200},
		{"GET", "/", "127.0.0.1", 200},
		{"GET", "/", "[::1]", 200},
		{"GET", "/", "janus.lan.ripdev.io", 200},
		{"GET", "/", "janus.local:8080", 200}, // port stripped before the check
		{"POST", "/", "janus.local", 405},
		{"POST", "/status.json", "janus.local", 405},
		{"PUT", "/status.json", "janus.local", 405},
		{"DELETE", "/", "janus.local", 405},
		{"GET", "/anything-else", "janus.local", 404},
		{"GET", "/status", "janus.local", 404},
		{"GET", "/", "evil.example.com", 421},
		{"GET", "/status.json", "attacker.test", 421},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://placeholder"+tc.path, nil)
		req.Host = tc.host
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != tc.want {
			t.Errorf("%s %s (Host %s) = %d, want %d", tc.method, tc.path, tc.host, rr.Code, tc.want)
		}
	}

	// Both routes answer no-store.
	for _, path := range []string{"/", "/status.json"} {
		req := httptest.NewRequest("GET", "http://placeholder"+path, nil)
		req.Host = "janus.local"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q", path, cc)
		}
	}
}

func TestMdnsHostAllowlistEffectiveName(t *testing.T) {
	app := newTestMdnsApp(t, &MdnsSettings{})
	// Simulate a conflict rename in the pooled advertiser.
	adv := app.state.mdns
	adv.mu.Lock()
	e := &mdnsEntry{name: "janus.local", typ: mdnsTypeFrontDoor, port: 80,
		state: mdnsStateRenamed, effective: "janus-2.local"}
	adv.entries[e.key()] = e
	adv.mu.Unlock()
	if !app.mdnsHostAllowed("janus-2.local") {
		t.Error("effective name rejected")
	}
	if !app.mdnsHostAllowed("janus.local") {
		t.Error("configured name rejected")
	}
	if app.mdnsHostAllowed("janus-3.local") {
		t.Error("unrelated name admitted")
	}
}

func TestMdnsSnapshotRedaction(t *testing.T) {
	app := newTestMdnsApp(t, &MdnsSettings{})
	rec, err := app.appsReg.create("shop", []string{"shop.local"}, "/rt/SECRET-bridge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.appsReg.setUpstreams(rec.ID, []Upstream{{Path: "/tmp/SENTINEL-xyz.sock"}}); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(app.mdnsStatusSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SENTINEL-xyz", "SECRET-bridge", "bridge_path", ".sock"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("status snapshot leaks %q: %s", secret, body)
		}
	}
	// The allowed shape is present.
	var snap mdnsStatusSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Apps) != 1 || snap.Apps[0].Upstreams.Total != 1 || snap.Apps[0].Upstreams.Healthy != 1 {
		t.Fatalf("snapshot apps: %+v", snap.Apps)
	}
	if snap.Apps[0].HeartbeatAgeMS < 0 || snap.Apps[0].HeartbeatAgeMS > 10_000 {
		t.Fatalf("heartbeat age: %d", snap.Apps[0].HeartbeatAgeMS)
	}
}

func TestHeartbeatAges(t *testing.T) {
	reg := newAppRegistry()
	now := time.Now()
	reg.now = func() time.Time { return now }
	rec, err := reg.create("shop", []string{"shop.local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * time.Second)
	if ages := reg.heartbeatAges(); ages[rec.ID] != 3*time.Second {
		t.Fatalf("age = %v", ages[rec.ID])
	}
	if err := reg.heartbeat(rec.ID); err != nil {
		t.Fatal(err)
	}
	if ages := reg.heartbeatAges(); ages[rec.ID] != 0 {
		t.Fatalf("age after re-stamp = %v", ages[rec.ID])
	}
}

func TestMdnsPageSelfContainedAndTextOnly(t *testing.T) {
	page := string(mdnsPageHTML)
	if strings.Contains(page, "innerHTML") {
		t.Error("status page uses innerHTML (contract: text nodes only)")
	}
	for _, external := range []string{"<link", "script src", "https://cdn", "@import"} {
		if strings.Contains(page, external) {
			t.Errorf("status page references an external resource (%q)", external)
		}
	}
	for _, required := range []string{"/status.json", "No apps registered", "textContent", "no-cors", "location.replace"} {
		if !strings.Contains(page, required) {
			t.Errorf("status page is missing %q", required)
		}
	}
}

// --- shared mode -----------------------------------------------------------------

// newTestSharedMdnsApp wires a shared-mode app (no listen): the decider
// compares against HTTP port 80, and the pooled advertiser carries a
// conflict-renamed configured name plus one hot-advertised app host.
func newTestSharedMdnsApp(t *testing.T) *App {
	t.Helper()
	app := newTestMdnsApp(t, &MdnsSettings{Canonical: "https://janus.lan.ripdev.io"})
	if !app.Mdns.shared() {
		t.Fatal("no listen must mean shared mode")
	}
	app.enableSharedFrontDoor(80)
	adv := app.state.mdns
	adv.mu.Lock()
	front := &mdnsEntry{name: "janus.local", typ: mdnsTypeFrontDoor, port: 80,
		state: mdnsStateRenamed, effective: "janus-2.local"}
	adv.entries[front.key()] = front
	shop := &mdnsEntry{name: "shop.local", app: "shop-x7k2p9", typ: mdnsTypeAppHost, port: 443,
		state: mdnsStateAnnounced, effective: "shop.local"}
	adv.entries[shop.key()] = shop
	adv.mu.Unlock()
	return app
}

// TestMdnsSharedHostMine pins the shared-mode live front-door set:
// configured name, effective (renamed) name, currently-advertised
// .local app hosts, and the canonical hostname are mine; IP literals
// and every other host are someone else's turn (never 421 — the
// decider passes them through).
func TestMdnsSharedHostMine(t *testing.T) {
	app := newTestSharedMdnsApp(t)
	cases := map[string]bool{
		"janus.local":         true,  // configured
		"janus-2.local":       true,  // effective after a conflict rename
		"shop.local":          true,  // hot-advertised app host
		"janus.lan.ripdev.io": true,  // canonical hostname (loop-guard page serves there)
		"127.0.0.1":           false, // IP literal: contract trade — pass-through in shared mode
		"::1":                 false,
		"192.168.1.10":        false,
		"other.ripdev.io":     false,
		"janus-3.local":       false,
	}
	for host, want := range cases {
		if got := app.mdnsSharedHostMine(host); got != want {
			t.Errorf("mine(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestMdnsSharedDecider pins the janus site handler as the shared-mode
// decider on the plain-HTTP port: front-door Hosts get the front door
// exactly as the dedicated listener serves it (page, /status.json,
// 404/405 discipline); everything else passes through to the next
// handler on the same server (the auto-HTTPS redirects) — never 421.
func TestMdnsSharedDecider(t *testing.T) {
	app := newTestSharedMdnsApp(t)
	h := &Handler{app: app, dp: app.dp, logger: zap.NewNop()}
	serve := func(method, path, host string, port int) (*httptest.ResponseRecorder, bool, error) {
		req := httptest.NewRequest(method, "http://placeholder"+path, nil)
		req.Host = host
		req = req.WithContext(context.WithValue(req.Context(),
			http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4zero, Port: port}))
		nextCalled := false
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			nextCalled = true
			w.WriteHeader(http.StatusTeapot) // a marker no janus path emits
			return nil
		})
		rr := httptest.NewRecorder()
		err := h.ServeHTTP(rr, req, next)
		return rr, nextCalled, err
	}

	mineCases := []struct {
		method, path, host string
		want               int
	}{
		{"GET", "/", "janus.local", 200},
		{"GET", "/status.json", "janus.local", 200},
		{"HEAD", "/status.json", "janus.local", 200},
		{"GET", "/", "janus-2.local", 200},          // renamed-mine
		{"GET", "/", "shop.local", 200},             // hot-advertised-mine
		{"GET", "/", "janus.lan.ripdev.io", 200},    // canonical-mine
		{"GET", "/", "janus.local:80", 200},         // port stripped before the check
		{"POST", "/status.json", "janus.local", 405},
		{"PUT", "/", "janus.local", 405},
		{"GET", "/anything-else", "janus.local", 404},
	}
	for _, tc := range mineCases {
		rr, nextCalled, err := serve(tc.method, tc.path, tc.host, 80)
		if err != nil {
			t.Errorf("%s %s (Host %s): %v", tc.method, tc.path, tc.host, err)
			continue
		}
		if nextCalled {
			t.Errorf("%s %s (Host %s) passed through instead of serving the front door", tc.method, tc.path, tc.host)
		}
		if rr.Code != tc.want {
			t.Errorf("%s %s (Host %s) = %d, want %d", tc.method, tc.path, tc.host, rr.Code, tc.want)
		}
	}

	// Not mine: pass through to next — including IP literals (the
	// shared-mode trade) — and never a 421.
	for _, host := range []string{"other.ripdev.io", "evil.example.com", "a.b.local",
		"127.0.0.1", "192.168.1.10", "[::1]"} {
		rr, nextCalled, err := serve("GET", "/", host, 80)
		if err != nil {
			t.Errorf("Host %s: %v", host, err)
			continue
		}
		if !nextCalled || rr.Code != http.StatusTeapot {
			t.Errorf("Host %s = %d (next called %v), want pass-through", host, rr.Code, nextCalled)
		}
	}

	// Another plain-HTTP port is not the shared front door: the decider
	// stays out and the data plane serves (unknown host → 404, next
	// never consulted).
	rr, nextCalled, err := serve("GET", "/", "janus.local", 8080)
	var herr caddyhttp.HandlerError
	if !errors.As(err, &herr) || herr.StatusCode != 404 {
		t.Fatalf("off-port request: err %v (code %d)", err, rr.Code)
	}
	if nextCalled {
		t.Fatal("off-port request passed through the decider")
	}
}

// TestMdnsSharedCoverage pins the hard Start error: shared mode with no
// http-port janus route whose host matcher covers the configured name
// refuses the config, and the error names the exact block to paste.
// Wildcard, explicit, and catch-all matchers all satisfy the check.
func TestMdnsSharedCoverage(t *testing.T) {
	server := func(listen string, patterns ...string) *caddyhttp.Server {
		var sets caddyhttp.MatcherSets
		if len(patterns) > 0 {
			sets = caddyhttp.MatcherSets{{caddyhttp.MatchHost(patterns)}}
		}
		return &caddyhttp.Server{
			Listen: []string{listen},
			Routes: caddyhttp.RouteList{{
				MatcherSets: sets,
				Handlers:    []caddyhttp.MiddlewareHandler{&Handler{}},
			}},
		}
	}
	cases := []struct {
		name string
		ha   *caddyhttp.App
		want bool
	}{
		{"wildcard covers", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": server(":80", "*.local")}}, true},
		{"explicit covers", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": server(":80", "janus.local")}}, true},
		{"catch-all covers", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": server(":80")}}, true},
		{"wrong host does not cover", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": server(":80", "example.com")}}, false},
		{"janus site on another port does not cover", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": server(":8080", "*.local")}}, false},
		{"no servers at all", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{}}, false},
		{"no janus route on the http port", &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
			"srv0": {Listen: []string{":80"}, Routes: caddyhttp.RouteList{}}}}, false},
	}
	for _, tc := range cases {
		if got := mdnsSharedSiteCovers(tc.ha, 80, "janus.local"); got != tc.want {
			t.Errorf("%s: covers = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The error text names the paste-this block and the dedicated remedy.
	err := mdnsSharedCoverageErr(80, "janus.local")
	for _, want := range []string{"janus.local", ":80", mdnsSharedPasteBlock, "`listen`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("coverage error %q missing %q", err.Error(), want)
		}
	}
}

// --- Start checks -----------------------------------------------------------------

func TestMdnsListenCollision(t *testing.T) {
	cases := []struct {
		front, server string
		want          bool
	}{
		{":80", ":80", true},
		{":80", "0.0.0.0:80", true},
		{"127.0.0.1:80", ":80", true},
		{":80", "127.0.0.1:80", true},
		{"127.0.0.1:80", "192.168.1.1:80", false},
		{"127.0.0.1:80", "127.0.0.1:80", true},
		{"localhost:80", "127.0.0.1:80", true}, // hostname listens resolve
		{"127.0.0.1:80", "localhost:80", true},
		{":7680", ":80", false},
		{":80", ":443", false},
		{":8443", ":8000-9000", true},
		{":7999", ":8000-9000", false},
		{":80", "unix//run/x.sock", false},
	}
	for _, tc := range cases {
		if got := mdnsListenCollides(tc.front, tc.server); got != tc.want {
			t.Errorf("collides(%q, %q) = %v, want %v", tc.front, tc.server, got, tc.want)
		}
	}
}

func TestMdnsStartRejectsMissingInterface(t *testing.T) {
	if err := validateMdnsInterfaces([]string{"en0"}); err != nil {
		t.Fatal(err)
	}
	app := newTestMdnsApp(t, &MdnsSettings{Interfaces: []string{"janus-test-does-not-exist0"}})
	// startMdns's first hard check: the pinned interface must exist.
	// (No listener is bound before the check fires.)
	err := app.startMdns()
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing pinned interface: %v", err)
	}
}
