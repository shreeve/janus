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
	"testing"

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
// fails to bind closes the first listener and stops the TTL sweeper — a
// rejected config must not leak a half-started app.
func TestStartUnwindsOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.sock")
	bad := filepath.Join(dir, strings.Repeat("x", 300)+".sock") // over the sun_path limit

	app := &App{
		Control: []Control{
			{Mode: "internal", network: "unix", addr: good, Listen: good},
			{Mode: "local", network: "unix", addr: bad, Listen: bad},
		},
		logger:  zap.NewNop(),
		appsReg: newAppRegistry(),
		hubs:    newHubSet(),
		ctx:     caddy.Context{Context: context.Background()},
	}

	if err := app.Start(); err == nil {
		t.Fatal("want Start to fail on the unbindable listener")
	}
	if len(app.controlSrvs) != 0 {
		t.Fatalf("want no leaked control servers, got %d", len(app.controlSrvs))
	}
	if _, err := net.Dial("unix", good); err == nil {
		t.Fatal("first listener still accepting after failed Start")
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
