package janus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// --- cold config: parse ------------------------------------------------------------

func TestParseHubDirectiveGlobal(t *testing.T) {
	d := caddyfile.NewTestDispenser(`janus {
		hub {
			path /realtime
			max_conns 16384
			max_frame 128kb
			max_channels 64
			origin same chat-admin.ripdev.io
		}
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	hs := app.Hub
	if hs == nil || hs.Enabled == nil || !*hs.Enabled {
		t.Fatal("hub with a block must be enabled")
	}
	if *hs.Path != "/realtime" || *hs.MaxConns != 16384 || *hs.MaxFrame != 128_000 || *hs.MaxChannels != 64 {
		t.Fatalf("knob parse: %+v", hs)
	}
	if len(hs.Origin) != 2 || hs.Origin[0] != "same" || hs.Origin[1] != "chat-admin.ripdev.io" {
		t.Fatalf("origin parse: %v", hs.Origin)
	}
}

func TestParseHubDirectiveForms(t *testing.T) {
	for _, cf := range []string{
		"janus {\n hub \n}",
		"janus {\n hub on \n}",
		"janus {\n hub off \n}",
		"janus {\n hub on {\n path /x \n} \n}",
		"janus {\n hub {\n origin any \n} \n}",
		"janus {\n hub {\n origin same \n} \n}",
		"janus {\n hub {\n origin a.example.com b.example.com \n} \n}",
	} {
		d := caddyfile.NewTestDispenser(cf)
		if err := new(App).UnmarshalCaddyfile(d); err != nil {
			t.Errorf("legal line rejected: %q: %v", cf, err)
		}
	}
}

// TestParseHubDirectiveHardErrors pins every hard-error row of the
// contract's "Hard errors (reject at parse)" list.
func TestParseHubDirectiveHardErrors(t *testing.T) {
	cases := []string{
		`janus { hub maybe }`,                        // unknown argument
		`janus { hub on off }`,                       // two arguments
		`janus { hub off { path /x } }`,              // block on an off switch
		`janus { hub { bogus 1 } }`,                  // unknown subdirective
		`janus { hub { path } }`,                     // path missing argument
		`janus { hub { path relative } }`,            // path not /-prefixed
		`janus { hub { path "/x?y" } }`,              // path with ?
		`janus { hub { path "/x#y" } }`,              // path with #
		`janus { hub { max_conns 0 } }`,              // not positive
		`janus { hub { max_conns -1 } }`,             // not positive
		`janus { hub { max_conns many } }`,           // not an integer
		`janus { hub { max_channels 0 } }`,           // not positive
		`janus { hub { max_frame 512b } }`,           // under 1kb
		`janus { hub { max_frame nope } }`,           // not a size
		`janus { hub { origin } }`,                   // no arguments
		`janus { hub { origin any same } }`,          // any is total
		`janus { hub { origin any a.example.com } }`, // any is total
		`janus { hub { origin "not a host" } }`,      // implausible hostname
		`janus { hub { path /x { nested } } }`,       // nested block
		`janus { hub { path /x path /y } }`,          // duplicate subdirective
		`janus { hub { } hub on }`,                   // duplicate hub directive
	}
	for _, cf := range cases {
		d := caddyfile.NewTestDispenser(cf)
		if err := new(App).UnmarshalCaddyfile(d); err == nil {
			t.Errorf("global parse accepted %q", cf)
		}
		d = caddyfile.NewTestDispenser(cf)
		var h Handler
		if err := h.UnmarshalCaddyfile(d); err == nil {
			t.Errorf("site parse accepted %q", cf)
		}
	}
}

// --- cascade -----------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

func TestHubCascade(t *testing.T) {
	sixteenK := 16384
	cases := []struct {
		name         string
		site, global *HubSettings
		wantOn       bool
		wantConns    int
	}{
		{"unset everywhere", nil, nil, false, 0},
		{"global on", nil, &HubSettings{Enabled: boolPtr(true)}, true, hubDefaultMaxConns},
		{"global on, site off", &HubSettings{Enabled: boolPtr(false)}, &HubSettings{Enabled: boolPtr(true)}, false, 0},
		{"global off, site on", &HubSettings{Enabled: boolPtr(true)}, &HubSettings{Enabled: boolPtr(false)}, true, hubDefaultMaxConns},
		{"site tunes one key", &HubSettings{Enabled: boolPtr(true), MaxConns: &sixteenK}, &HubSettings{Enabled: boolPtr(true)}, true, 16384},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			app := &App{Hub: tt.global}
			h := &Handler{Hub: tt.site, app: app, dp: &dataPlane{}}
			if err := h.provisionHub(); err != nil {
				t.Fatal(err)
			}
			if (h.hubCfg != nil) != tt.wantOn {
				t.Fatalf("effective on: want %v, got %v", tt.wantOn, h.hubCfg != nil)
			}
			if tt.wantOn && h.hubCfg.maxConns != tt.wantConns {
				t.Fatalf("max_conns: want %d, got %d", tt.wantConns, h.hubCfg.maxConns)
			}
		})
	}

	// Unmentioned keys inherit the global values; named keys override.
	path := "/rt"
	frame := int64(128 << 10)
	conns := 99
	app := &App{Hub: &HubSettings{Enabled: boolPtr(true), Path: &path, MaxFrame: &frame}}
	h := &Handler{Hub: &HubSettings{Enabled: boolPtr(true), MaxConns: &conns}, app: app, dp: &dataPlane{}}
	if err := h.provisionHub(); err != nil {
		t.Fatal(err)
	}
	if h.hubCfg.path != "/rt" || h.hubCfg.maxFrame != 128<<10 || h.hubCfg.maxConns != 99 || h.hubCfg.maxChannels != hubDefaultMaxChannels {
		t.Fatalf("per-key cascade: %+v", h.hubCfg)
	}

	// Origin cascade: site origin replaces the global posture wholesale.
	originAny := &HubSettings{Enabled: boolPtr(true), Origin: []string{"any"}}
	originList := &HubSettings{Enabled: boolPtr(true), Origin: []string{"same", "admin.example.com"}}
	h = &Handler{Hub: originList, app: &App{Hub: originAny}, dp: &dataPlane{}}
	if err := h.provisionHub(); err != nil {
		t.Fatal(err)
	}
	if h.hubCfg.originAny || !h.hubCfg.originSame || !h.hubCfg.originHosts["admin.example.com"] {
		t.Fatalf("origin cascade: %+v", h.hubCfg)
	}
}

// --- origin policy ------------------------------------------------------------------

func TestHubOriginPolicy(t *testing.T) {
	mk := func(origin string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "https://chat.example.com/hub", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	same := &hubSite{originSame: true, originHosts: map[string]bool{}}
	if !hubOriginAllowed(same, mk("https://chat.example.com"), "chat.example.com") {
		t.Fatal("same-host origin must pass")
	}
	if hubOriginAllowed(same, mk("https://evil.example.com"), "chat.example.com") {
		t.Fatal("cross-site origin must fail")
	}
	if hubOriginAllowed(same, mk(""), "chat.example.com") {
		t.Fatal("missing Origin must fail `same`")
	}
	// Scheme is not compared; ports are ignored.
	if !hubOriginAllowed(same, mk("http://chat.example.com:8443"), "chat.example.com") {
		t.Fatal("scheme/port must not defeat same")
	}

	allow := &hubSite{originSame: true, originHosts: map[string]bool{"admin.example.com": true}}
	if !hubOriginAllowed(allow, mk("https://admin.example.com"), "chat.example.com") {
		t.Fatal("allowlisted host must pass")
	}

	any := &hubSite{originAny: true}
	if !hubOriginAllowed(any, mk(""), "chat.example.com") {
		t.Fatal("`any` must admit missing Origin")
	}
}

// --- host matching and the floor -----------------------------------------------------

func TestHostMatchesPattern(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"chat.ripdev.io", "chat.ripdev.io", true},
		{"chat.ripdev.io", "other.ripdev.io", false},
		{"*.ripdev.io", "chat.ripdev.io", true},
		{"*.ripdev.io", "a.b.ripdev.io", false}, // one label per *
		{"*.ripdev.io", "ripdev.io", false},
		{"*", "anything", true},
	}
	for _, tt := range cases {
		if got := hostMatchesPattern(tt.pattern, tt.host); got != tt.want {
			t.Errorf("match(%q, %q): want %v, got %v", tt.pattern, tt.host, tt.want, got)
		}
	}
}

func TestHubMaxConnsFloor(t *testing.T) {
	ten := &hubSite{maxConns: 10}
	twenty := &hubSite{maxConns: 20}
	app := &App{hubSites: [][]hubSiteEntry{{
		{patterns: []string{"ten.example.com"}, cfg: ten},
		{patterns: []string{"twenty.example.com"}, cfg: twenty},
		{patterns: []string{"off.example.com"}, cfg: nil},
	}}}

	// One app spanning hosts capped 10 and 20 admits at most 10.
	rec := AppRecord{Hosts: []string{"ten.example.com", "twenty.example.com"}}
	floor, enabled := app.hubMaxConnsFloor(rec)
	if !enabled || floor != 10 {
		t.Fatalf("floor: want 10, got %d (%v)", floor, enabled)
	}

	// Hub-off hosts contribute nothing.
	rec = AppRecord{Hosts: []string{"off.example.com", "twenty.example.com"}}
	floor, enabled = app.hubMaxConnsFloor(rec)
	if !enabled || floor != 20 {
		t.Fatalf("floor with off host: want 20, got %d", floor)
	}

	// No hub-enabled host → not enabled (the publish plane's 409).
	rec = AppRecord{Hosts: []string{"off.example.com", "unknown.example.com"}}
	if _, enabled := app.hubMaxConnsFloor(rec); enabled {
		t.Fatal("hubless app must not be enabled")
	}

	// First match per server wins: a specific site shadows the catchall
	// that follows it in specificity order.
	app = &App{hubSites: [][]hubSiteEntry{{
		{patterns: []string{"special.example.com"}, cfg: nil}, // hub off
		{patterns: []string{"*.example.com"}, cfg: ten},
	}}}
	rec = AppRecord{Hosts: []string{"special.example.com"}}
	if _, enabled := app.hubMaxConnsFloor(rec); enabled {
		t.Fatal("explicit off site must shadow the catchall")
	}
}

// --- registry: bridge_path ------------------------------------------------------------

func TestBridgePathValidation(t *testing.T) {
	r := newAppRegistry()
	bad := []string{
		"rt/bridge",                    // no leading /
		"/rt bridge",                   // whitespace
		"/rt?x=1",                      // query
		"/rt#frag",                     // fragment
		"/rt\tbridge",                  // control
		"/" + strings.Repeat("x", 256), // too long
	}
	for _, p := range bad {
		if _, err := r.create("shop", []string{"shop.example.com"}, p); err == nil {
			t.Errorf("create accepted bridge_path %q", p)
		}
	}
	rec, err := r.create("shop", []string{"shop.example.com"}, "/rt/bridge")
	if err != nil {
		t.Fatal(err)
	}
	if rec.BridgePath != "/rt/bridge" {
		t.Fatalf("bridge_path not stored: %+v", rec)
	}

	// PATCH set, change, clear.
	newPath := "/hooks/ws"
	rec, err = r.patch(rec.ID, nil, nil, &newPath)
	if err != nil || rec.BridgePath != "/hooks/ws" {
		t.Fatalf("patch set: %v %+v", err, rec)
	}
	empty := ""
	rec, err = r.patch(rec.ID, nil, nil, &empty)
	if err != nil || rec.BridgePath != "" {
		t.Fatalf("patch clear: %v %+v", err, rec)
	}
	badPath := "no-slash"
	if _, err := r.patch(rec.ID, nil, nil, &badPath); err == nil {
		t.Fatal("patch accepted an illegal bridge_path")
	}

	// GET surfaces it.
	if _, err := r.patch(rec.ID, nil, nil, &newPath); err != nil {
		t.Fatal(err)
	}
	got, err := r.get(rec.ID)
	if err != nil || got.BridgePath != "/hooks/ws" {
		t.Fatalf("get: %v %+v", err, got)
	}
}

func TestBridgePathPatchTriState(t *testing.T) {
	// absent → nil (unchanged)
	if p, err := bridgePathPatch(nil); err != nil || p != nil {
		t.Fatalf("absent: %v %v", p, err)
	}
	// null → clear
	p, err := bridgePathPatch([]byte("null"))
	if err != nil || p == nil || *p != "" {
		t.Fatalf("null: %v %v", p, err)
	}
	// string → set
	p, err = bridgePathPatch([]byte(`"/rt"`))
	if err != nil || p == nil || *p != "/rt" {
		t.Fatalf("string: %v %v", p, err)
	}
	// wrong type → 400
	if _, err := bridgePathPatch([]byte("42")); err == nil {
		t.Fatal("number must reject")
	}
}

// --- UsagePool reload persistence -------------------------------------------------------

// TestJanusStatePooling pins that repeated acquisitions share one live
// state (no split-brain registry) and that releasing one reference —
// a config reload retiring its old app — never destructs the holder.
func TestJanusStatePooling(t *testing.T) {
	key := "janus.test." + t.Name()
	build := func() (*janusState, error) {
		st, _, err := janusPool.LoadOrNew(key, func() (caddy.Destructor, error) {
			return newJanusState(nil, 0)
		})
		if err != nil {
			return nil, err
		}
		return st.(*janusState), nil
	}
	st1, err := build()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := st1.registry.create("keep", []string{"keep.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	hub := st1.hubs.getOrCreate(rec.ID, nil)

	// The "reload": a second config generation acquires the state.
	st2, err := build()
	if err != nil {
		t.Fatal(err)
	}
	if st2 != st1 {
		t.Fatal("second acquisition must return the pooled state")
	}
	if _, err := st2.registry.get(rec.ID); err != nil {
		t.Fatal("registration must survive the second acquisition")
	}
	if st2.hubs.getOrCreate(rec.ID, nil) != hub {
		t.Fatal("hub entry must survive the second acquisition")
	}

	// The old generation releases: state lives on for the new one.
	if _, err := janusPool.Delete(key); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.registry.get(rec.ID); err != nil {
		t.Fatal("registration must survive releasing one reference")
	}
	if st2.registry.sweepStop == nil {
		t.Fatal("sweeper must keep running while a reference remains")
	}

	// The last release destructs: sweeper stopped, hubs torn down.
	if _, err := janusPool.Delete(key); err != nil {
		t.Fatal(err)
	}
	if st2.registry.sweepStop != nil {
		t.Fatal("last release must stop the sweeper")
	}
}
