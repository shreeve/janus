package janus

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

func TestParseTokenArg(t *testing.T) {
	tests := []struct {
		val     string
		quoted  bool
		kind    string
		ref     string
		wantErr bool
	}{
		{"token:JANUS_TOKEN", false, tokenEnv, "JANUS_TOKEN", false},
		{"token:./secrets/x", false, tokenFile, "./secrets/x", false},
		{"token:dev-secret", true, tokenLiteral, "dev-secret", false},
		{"token:", false, "", "", true},
		{"nope", false, "", "", true},
	}
	for _, tt := range tests {
		kind, ref, err := parseTokenArg(tt.val, tt.quoted)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("%q: want error", tt.val)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%q: %v", tt.val, err)
		}
		if kind != tt.kind || ref != tt.ref {
			t.Fatalf("%q: got (%s,%s), want (%s,%s)", tt.val, kind, ref, tt.kind, tt.ref)
		}
	}
}

func TestControlRejectsUnsafeListenURLs(t *testing.T) {
	for _, listen := range []string{
		"http://user@127.0.0.1:7600/admin",
		"http://127.0.0.1:7600/admin?debug=1",
		"http://127.0.0.1:7600/admin#fragment",
		"http://127.0.0.1:7600/{",
		"http://127.0.0.1:7600/%7Bname%7D",
		"http://127.0.0.1:7600/%61dmin",
		"http://127.0.0.1:7600/%25admin",
		"http://127.0.0.1:7600/admin%20api",
		"http://127.0.0.1:7600/admin//v1",
		"http://127.0.0.1:7600/admin/../ops",
	} {
		c := Control{Mode: "local", Listen: listen}
		if err := c.normalize(); err == nil {
			t.Errorf("accepted unsafe listen URL %q", listen)
		}
	}
}

func TestNormalizeControlsRejectsEffectiveBindCollision(t *testing.T) {
	t.Setenv("JANUS_TEST_TOKEN", "secret")
	for _, publicListen := range []string{
		"https://127.0.0.1:17601/public",
		"https://0.0.0.0:17601/public",
		"https://localhost:17601/public",
	} {
		controls := []Control{
			{Mode: "local", Listen: "https://127.0.0.1:17601/local"},
			{Mode: "public", Listen: publicListen, TokenKind: tokenEnv, Token: "JANUS_TEST_TOKEN"},
		}
		err := normalizeControls(controls)
		if err == nil || !strings.Contains(err.Error(), "overlapping tcp addresses") {
			t.Errorf("want effective-bind collision for %q, got %v", publicListen, err)
		}
	}
}

func TestControlMuxBaseIsolation(t *testing.T) {
	app := &App{}
	for _, tt := range []struct {
		name      string
		mux       http.Handler
		served    string
		notServed string
	}{
		{name: "root", mux: app.controlMuxAt(""), served: "/1.0", notServed: "/admin/1.0"},
		{name: "prefixed", mux: app.controlMuxAt("/admin"), served: "/admin/1.0", notServed: "/1.0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			served := httptest.NewRecorder()
			tt.mux.ServeHTTP(served, httptest.NewRequest(http.MethodGet, tt.served, nil))
			if served.Code != http.StatusOK {
				t.Fatalf("%s: got %d, want 200", tt.served, served.Code)
			}
			missing := httptest.NewRecorder()
			tt.mux.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, tt.notServed, nil))
			if missing.Code != http.StatusNotFound {
				t.Fatalf("%s: got %d, want 404", tt.notServed, missing.Code)
			}
		})
	}
}

type closeCountingListener struct {
	net.Listener
	closes atomic.Int32
}

func (ln *closeCountingListener) Close() error {
	ln.closes.Add(1)
	return ln.Listener.Close()
}

func TestIdempotentListenerClosesCaddyHandleOnce(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &closeCountingListener{Listener: raw}
	ln := &idempotentListener{Listener: counted}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Serve did not return after listener close")
	}
	if got := counted.closes.Load(); got != 1 {
		t.Fatalf("underlying listener closed %d times, want 1", got)
	}
}

func TestResolveToken(t *testing.T) {
	t.Setenv("JANUS_TEST_TOKEN", "from-env")
	got, err := resolveToken(tokenEnv, "JANUS_TEST_TOKEN")
	if err != nil || got != "from-env" {
		t.Fatalf("env: got %q err %v", got, err)
	}
	if _, err := resolveToken(tokenEnv, "JANUS_TEST_TOKEN_MISSING"); err == nil {
		t.Fatal("missing env: want error")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "auth")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = resolveToken(tokenFile, path)
	if err != nil || got != "from-file" {
		t.Fatalf("file: got %q err %v", got, err)
	}

	got, err = resolveToken(tokenLiteral, "lit")
	if err != nil || got != "lit" {
		t.Fatalf("literal: got %q err %v", got, err)
	}
}

func TestControlNormalizeDefaults(t *testing.T) {
	c := Control{Mode: "internal"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultControlInternal || c.network != "unix" || c.addr != DefaultControlInternal {
		t.Fatalf("internal defaults: %+v", c)
	}

	c = Control{Mode: "local"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultControlLocal || c.network != "tcp" || c.addr != "127.0.0.1:7600" || c.useTLS {
		t.Fatalf("local defaults: %+v", c)
	}
}

func TestControlNormalizePublic(t *testing.T) {
	t.Setenv("JANUS_PUB", "secret")
	c := Control{Mode: "public", TokenKind: tokenEnv, Token: "JANUS_PUB"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if !c.useTLS || c.addr != "0.0.0.0:7601" || c.secret != "secret" {
		t.Fatalf("public defaults: %+v secret=%q", c, c.secret)
	}

	c = Control{Mode: "public"}
	if err := c.normalize(); err == nil {
		t.Fatal("public without token: want error")
	}

	c = Control{Mode: "public", TokenKind: tokenLiteral, Token: "nope"}
	if err := c.normalize(); err == nil {
		t.Fatal("public literal token: want error")
	}

	c = Control{Mode: "public", Listen: "http://0.0.0.0:7601/", TokenKind: tokenEnv, Token: "JANUS_PUB"}
	if err := c.normalize(); err == nil {
		t.Fatal("public http: want error")
	}
}

func TestControlNormalizeCertKey(t *testing.T) {
	t.Setenv("JANUS_PUB", "secret")

	// Both on a TLS listener: accepted, carried through to Start.
	c := Control{Mode: "public", TokenKind: tokenEnv, Token: "JANUS_PUB",
		CertFile: "/etc/janus/tls.crt", KeyFile: "/etc/janus/tls.key"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.CertFile != "/etc/janus/tls.crt" || c.KeyFile != "/etc/janus/tls.key" {
		t.Fatalf("cert/key mangled: %+v", c)
	}

	// One without the other is a hard error, in both directions.
	c = Control{Mode: "public", TokenKind: tokenEnv, Token: "JANUS_PUB", CertFile: "/etc/janus/tls.crt"}
	if err := c.normalize(); err == nil {
		t.Fatal("cert without key: want error")
	}
	c = Control{Mode: "public", TokenKind: tokenEnv, Token: "JANUS_PUB", KeyFile: "/etc/janus/tls.key"}
	if err := c.normalize(); err == nil {
		t.Fatal("key without cert: want error")
	}

	// Only meaningful with TLS: plain-HTTP local and internal reject loudly.
	c = Control{Mode: "local", CertFile: "/x.crt", KeyFile: "/x.key"}
	if err := c.normalize(); err == nil {
		t.Fatal("cert/key on plain-http local: want error")
	}
	c = Control{Mode: "internal", CertFile: "/x.crt", KeyFile: "/x.key"}
	if err := c.normalize(); err == nil {
		t.Fatal("cert/key on internal: want error")
	}

	// An https:// local listener is TLS: cert/key are legal there.
	c = Control{Mode: "local", Listen: "https://127.0.0.1:7600/", CertFile: "/x.crt", KeyFile: "/x.key"}
	if err := c.normalize(); err != nil {
		t.Fatalf("cert/key on https local: %v", err)
	}
}

func TestControlNormalizeLocalLoopback(t *testing.T) {
	c := Control{Mode: "local", Listen: "http://192.168.1.1:7600/"}
	if err := c.normalize(); err == nil {
		t.Fatal("non-loopback local: want error")
	}
}

func TestControlNormalizeBasePath(t *testing.T) {
	c := Control{Mode: "local", Listen: "http://127.0.0.1:7600/admin/"}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.basePath != "/admin" {
		t.Fatalf("base path: got %q", c.basePath)
	}
}

func TestParseControlCaddyfile(t *testing.T) {
	t.Setenv("JANUS_PUB", "x")
	d := caddyfile.NewTestDispenser(`janus {
		control internal
		control local
		control public token:JANUS_PUB
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if len(app.Control) != 3 {
		t.Fatalf("want 3 controls, got %d", len(app.Control))
	}
	if app.Control[0].Mode != "internal" || app.Control[1].Mode != "local" || app.Control[2].Mode != "public" {
		t.Fatalf("modes: %+v", app.Control)
	}
	if app.Control[2].TokenKind != tokenEnv || app.Control[2].Token != "JANUS_PUB" {
		t.Fatalf("public token: %+v", app.Control[2])
	}
}

func TestParseControlQuotedLiteral(t *testing.T) {
	d := caddyfile.NewTestDispenser(`janus {
		control local "token:dev-only"
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if len(app.Control) != 1 || app.Control[0].TokenKind != tokenLiteral || app.Control[0].Token != "dev-only" {
		t.Fatalf("got %+v", app.Control)
	}
}

func TestParseControlCertKey(t *testing.T) {
	t.Setenv("JANUS_PUB", "x")
	d := caddyfile.NewTestDispenser(`janus {
		control public token:JANUS_PUB cert:/etc/janus/tls.crt key:/etc/janus/tls.key
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if len(app.Control) != 1 {
		t.Fatalf("want 1 control, got %d", len(app.Control))
	}
	c := app.Control[0]
	if c.CertFile != "/etc/janus/tls.crt" || c.KeyFile != "/etc/janus/tls.key" {
		t.Fatalf("cert/key: %+v", c)
	}

	for name, src := range map[string]string{
		"empty cert":     `janus { control public token:JANUS_PUB cert: key:/k }`,
		"empty key":      `janus { control public token:JANUS_PUB cert:/c key: }`,
		"duplicate cert": `janus { control public token:JANUS_PUB cert:/c cert:/c2 key:/k }`,
		"duplicate key":  `janus { control public token:JANUS_PUB cert:/c key:/k key:/k2 }`,
	} {
		app := new(App)
		if err := app.UnmarshalCaddyfile(caddyfile.NewTestDispenser(src)); err == nil {
			t.Fatalf("%s: want parse error", name)
		}
	}
}

func TestParseControlRejectsSiblingToken(t *testing.T) {
	d := caddyfile.NewTestDispenser(`janus {
		token JANUS_TOKEN
	}`)
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err == nil {
		t.Fatal("want error for sibling token directive")
	}
}

func TestAppDefaultInternalWhenEmpty(t *testing.T) {
	app := &App{}
	if len(app.Control) != 0 {
		t.Fatal("precondition")
	}
	// Mirror Provision injection + normalize (without caddy.Context).
	if len(app.Control) == 0 {
		app.Control = []Control{{Mode: "internal"}}
	}
	if err := app.Control[0].normalize(); err != nil {
		t.Fatal(err)
	}
	if app.Control[0].Mode != "internal" || app.Control[0].Listen != DefaultControlInternal {
		t.Fatalf("got %+v", app.Control[0])
	}
}

// TestStartUnwindsOnPartialFailure pins that a Start whose second listener
// fails to bind stops serving on the first listener and stops the TTL sweeper —
// a rejected config must not leak a half-started app.
func TestStartUnwindsOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.sock")
	bad := filepath.Join(dir, strings.Repeat("x", 300)+".sock") // over the sun_path limit

	app := &App{
		Control: []Control{
			{Mode: "internal", network: "unix", addr: good, Listen: good},
			{Mode: "local", network: "unix", addr: bad, Listen: bad},
		},
		logger:        zap.NewNop(),
		appsReg:       newAppRegistry(),
		hubs:          newHubSet(),
		ctx:           caddy.Context{Context: context.Background()},
		accessStreams: make(map[*accessSubscriber]struct{}),
	}
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	app.access = bridge
	state := bridge.newState()
	sub := &accessSubscriber{state: state, lines: make(chan *accessLine, 1), done: make(chan struct{})}
	state.subscribers[sub] = struct{}{}
	bridge.subscribers = 1
	app.accessStreams[sub] = struct{}{}

	if err := app.Start(); err == nil {
		t.Fatal("want Start to fail on the unbindable listener")
	}
	if len(app.controlSrvs) != 0 {
		t.Fatalf("want no leaked control servers, got %d", len(app.controlSrvs))
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", good)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 250 * time.Millisecond}
	if response, err := client.Get("http://janus/1.0"); err == nil {
		response.Body.Close()
		t.Fatal("first listener still serving after failed Start")
	}
	if !sub.detached || sub.reason != "generation_stop" {
		t.Fatalf("failed Start left subscriber attached: %+v", sub)
	}
	select {
	case <-sub.done:
	default:
		t.Fatal("failed Start did not wake subscriber")
	}
	if bridge.subscribers != 0 || bridge.counters.streamCloses.Load() != 1 {
		t.Fatalf("failed Start subscriber accounting: subscribers=%d closes=%d",
			bridge.subscribers, bridge.counters.streamCloses.Load())
	}
}

func TestStartUnwindsStreamsBeforeControlOnMdnsFailure(t *testing.T) {
	dir, err := os.MkdirTemp("", "janus-start")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	good := filepath.Join(dir, "good.sock")
	app := newTestMdnsApp(t, &MdnsSettings{Interfaces: []string{"janus-test-does-not-exist0"}})
	app.Control = []Control{{Mode: "internal", network: "unix", addr: good, Listen: good}}
	app.logger = zap.NewNop()
	app.ctx = caddy.Context{Context: context.Background()}
	app.accessStreams = make(map[*accessSubscriber]struct{})
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.appsReg.bindAccess(bridge); err != nil {
		t.Fatal(err)
	}
	app.access = bridge
	state := bridge.newState()
	sub := &accessSubscriber{state: state, lines: make(chan *accessLine, 1), done: make(chan struct{})}
	state.subscribers[sub] = struct{}{}
	bridge.subscribers = 1
	app.accessStreams[sub] = struct{}{}

	if err := app.Start(); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want mDNS interface failure, got %v", err)
	}
	if len(app.controlSrvs) != 0 {
		t.Fatalf("want no leaked control servers, got %d", len(app.controlSrvs))
	}
	if !sub.detached || sub.reason != "generation_stop" {
		t.Fatalf("failed mDNS start left subscriber attached: %+v", sub)
	}
	select {
	case <-sub.done:
	default:
		t.Fatal("failed mDNS start did not wake subscriber")
	}
	if bridge.subscribers != 0 || bridge.counters.streamCloses.Load() != 1 {
		t.Fatalf("failed mDNS start subscriber accounting: subscribers=%d closes=%d",
			bridge.subscribers, bridge.counters.streamCloses.Load())
	}
}

func TestBearerAuth(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := bearerAuth("sekrit", ok)

	req := httptest.NewRequest(http.MethodGet, "/1.0", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/1.0", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad auth: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/1.0", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("good auth: %d", rr.Code)
	}
}

func TestControlMuxRejectsUnknownPaths(t *testing.T) {
	app := &App{
		Control: []Control{{Mode: "local", Listen: DefaultControlLocal}},
	}
	if err := app.Control[0].normalize(); err != nil {
		t.Fatal(err)
	}
	mux := app.controlMux()

	do := func(method, path string) int {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		return rr.Code
	}

	// Garbage under /1.0 must never answer 200.
	if code := do(http.MethodGet, "/1.0/bogus"); code != http.StatusNotFound {
		t.Fatalf("GET /1.0/bogus: want 404, got %d", code)
	}
	// A method bug (GET where the route is POST-only) must never look alive.
	if code := do(http.MethodGet, "/1.0/apps/x/heartbeat"); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /1.0/apps/x/heartbeat: want 405, got %d", code)
	}
	if code := do(http.MethodGet, "/1.0/health/bogus"); code != http.StatusNotFound {
		t.Fatalf("GET /1.0/health/bogus: want 404, got %d", code)
	}
	// The root itself stays GET/HEAD-only, with and without the slash.
	if code := do(http.MethodGet, "/1.0/"); code != http.StatusOK {
		t.Fatalf("GET /1.0/: want 200, got %d", code)
	}
	if code := do(http.MethodHead, "/1.0"); code != http.StatusOK {
		t.Fatalf("HEAD /1.0: want 200, got %d", code)
	}
	if code := do(http.MethodPost, "/1.0"); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /1.0: want 405, got %d", code)
	}
}

func TestControlAPIHandlers(t *testing.T) {
	on := true
	app := &App{
		Ping: &on,
		Control: []Control{
			{Mode: "local", Listen: DefaultControlLocal},
		},
	}
	if err := app.Control[0].normalize(); err != nil {
		t.Fatal(err)
	}
	mux := app.controlMux()

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/1.0", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/1.0: %d", rr.Code)
	}
	var root map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root["api_version"] != "1.0" || root["type"] != "janus" {
		t.Fatalf("root body: %v", root)
	}
	if root["ping"] != true {
		t.Fatalf("ping: %v", root["ping"])
	}
	// mdns is presence-shaped like ping: the boolean rides GET /1.0.
	if root["mdns"] != false {
		t.Fatalf("mdns: %v", root["mdns"])
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/1.0/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/1.0/health: %d body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"ok"`) && !strings.Contains(rr.Body.String(), `"status": "ok"`) {
		// json encoder has no spaces
		var health map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &health); err != nil || health["status"] != "ok" {
			t.Fatalf("health: %s", rr.Body.String())
		}
	}
}
