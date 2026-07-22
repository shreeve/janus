package janus

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brutella/dnssd"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
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
	if app.Mdns.Name != "janus.local" || app.Mdns.Listen != ":80" || !app.Mdns.appsOn() {
		t.Fatalf("defaults: %+v", app.Mdns)
	}
	if app.Mdns.listenPort() != 80 {
		t.Fatalf("listenPort: %d", app.Mdns.listenPort())
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

type fakeHandle struct{ srv dnssd.Service }

func (h *fakeHandle) UpdateText(map[string]string, dnssd.Responder) {}
func (h *fakeHandle) Service() dnssd.Service                        { return h.srv }

type fakeResponder struct {
	mu       sync.Mutex
	adds     []string
	removes  []string
	renameTo map[string]string // desired host label → renamed label
	addDelay time.Duration
}

func (f *fakeResponder) Add(srv dnssd.Service) (dnssd.ServiceHandle, error) {
	if f.addDelay > 0 {
		time.Sleep(f.addDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.adds = append(f.adds, srv.Host+"."+srv.Domain)
	if to, ok := f.renameTo[srv.Host]; ok {
		srv.Host = to
	}
	return &fakeHandle{srv}, nil
}

func (f *fakeResponder) Remove(h dnssd.ServiceHandle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	srv := h.Service()
	f.removes = append(f.removes, srv.Host+"."+srv.Domain)
}

func (f *fakeResponder) Respond(ctx context.Context) error {
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
	if err := adv.configure(cfg); err != nil {
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
	if err := adv.configure(&mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
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
	if err := adv.configure(&mdnsConfig{name: "edge.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	snap = adv.snapshot("edge.local")
	if len(snap.entries) != 1 || snap.entries[0].Name != "edge.local" {
		t.Fatalf("rename config: %+v", snap.entries)
	}

	// Disable: teardown clears everything and counts the withdrawals.
	w0 := snap.withdraws
	if err := adv.configure(nil); err != nil {
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
	if err := adv.configure(&mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
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

func TestMdnsSkippedGaugeLifecycle(t *testing.T) {
	reg := newAppRegistry()
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(&mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
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
	if err := adv.configure(&mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
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

func TestMdnsFrontDoorRouting(t *testing.T) {
	app := newTestMdnsApp(t, &MdnsSettings{Canonical: "https://janus.lan.ripdev.io"})
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
