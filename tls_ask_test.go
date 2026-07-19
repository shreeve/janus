package janus

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// askCode queries GET /1.0/tls/ask?domain=… (rawQuery passed verbatim so
// tests can omit or empty the parameter) and returns the status code.
func askCode(t *testing.T, mux *http.ServeMux, rawQuery string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/1.0/tls/ask?"+rawQuery, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr.Code
}

func TestTLSAskAllowedAndUnknown(t *testing.T) {
	mux := newTestControlMux(t)
	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusNotFound {
		t.Fatalf("unregistered domain: want 404, got %d", code)
	}

	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}

	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusOK {
		t.Fatalf("registered domain: want 200, got %d", code)
	}
	if code := askCode(t, mux, "domain=other.example.com"); code != http.StatusNotFound {
		t.Fatalf("other domain: want 404, got %d", code)
	}
}

func TestTLSAskResponseNamesTheApp(t *testing.T) {
	mux := newTestControlMux(t)
	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id, _ := body["id"].(string)

	code, body = doJSON(t, mux, http.MethodGet, "/1.0/tls/ask?domain=shop.example.com", "")
	if code != http.StatusOK {
		t.Fatalf("ask: want 200, got %d %v", code, body)
	}
	if body["domain"] != "shop.example.com" || body["app"] != id {
		t.Fatalf("ask body: %v", body)
	}
}

func TestTLSAskNormalizesDomain(t *testing.T) {
	mux := newTestControlMux(t)
	if code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	// The registry stores lowercase; the ask normalizes the query the same
	// way the data plane normalizes Host (lowercase, trailing dot stripped).
	for _, q := range []string{
		"domain=SHOP.Example.COM",
		"domain=shop.example.com.",
	} {
		if code := askCode(t, mux, q); code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", q, code)
		}
	}
}

func TestTLSAskMissingDomain400(t *testing.T) {
	mux := newTestControlMux(t)
	for _, q := range []string{"", "domain="} {
		if code := askCode(t, mux, q); code != http.StatusBadRequest {
			t.Fatalf("query %q: want 400, got %d", q, code)
		}
	}
}

func TestTLSAskGetOnly(t *testing.T) {
	mux := newTestControlMux(t)
	req := httptest.NewRequest(http.MethodPost, "/1.0/tls/ask?domain=shop.example.com", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST ask: want 405, got %d", rr.Code)
	}
}

func TestTLSAskWildcardNeverMatches(t *testing.T) {
	mux := newTestControlMux(t)
	// Wildcard hosts cannot enter the registry (Phase 3 validation rejects
	// "*" in labels), so a wildcard ask can never be allowed.
	if code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["*.example.com"]}`); code != http.StatusBadRequest {
		t.Fatalf("wildcard registration: want 400, got %d", code)
	}
	if code := askCode(t, mux, "domain=*.example.com"); code != http.StatusNotFound {
		t.Fatalf("wildcard ask: want 404, got %d", code)
	}
}

// newClockedControlMux is newTestControlMux with an injectable heartbeat
// clock, for driving register→delete→reap transitions deterministically.
func newClockedControlMux(t *testing.T, ttl time.Duration) (*http.ServeMux, *appRegistry, *fakeClock) {
	t.Helper()
	app := &App{Control: []Control{{Mode: "local", Listen: DefaultControlLocal}}}
	if err := app.Control[0].normalize(); err != nil {
		t.Fatal(err)
	}
	reg, clk := newClockedRegistry(t, ttl)
	app.appsReg = reg
	return app.controlMux(), reg, clk
}

func TestTLSAskFollowsRegistryLifecycle(t *testing.T) {
	mux, reg, clk := newClockedControlMux(t, 15*time.Second)

	// register → allowed
	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusOK {
		t.Fatalf("after register: want 200, got %d", code)
	}

	// DELETE → denied
	if code, _ := doJSON(t, mux, http.MethodDelete, "/1.0/apps/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusNotFound {
		t.Fatalf("after delete: want 404, got %d", code)
	}

	// re-register → allowed again; TTL reap → denied again
	if code, _ = doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`); code != http.StatusCreated {
		t.Fatalf("re-create: %d", code)
	}
	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusOK {
		t.Fatalf("after re-register: want 200, got %d", code)
	}
	clk.advance(16 * time.Second)
	if reaped := reg.sweepExpired(); len(reaped) != 1 {
		t.Fatalf("want one reap, got %v", reaped)
	}
	if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusNotFound {
		t.Fatalf("after reap: want 404, got %d", code)
	}
}

func TestTLSAskAliveButUnroutableStaysAllowed(t *testing.T) {
	// Heartbeat ≠ readiness: empty upstreams with fresh heartbeats keeps
	// the cert allowance — a reload must not break TLS.
	mux, reg, clk := newClockedControlMux(t, 15*time.Second)
	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if code, _ := doJSON(t, mux, http.MethodPut, "/1.0/apps/"+id+"/upstreams",
		`{"upstreams":[]}`); code != http.StatusOK {
		t.Fatalf("put empty upstreams: %d", code)
	}

	for i := 0; i < 10; i++ { // 50s of 5s heartbeats, over three TTLs
		clk.advance(5 * time.Second)
		if err := reg.heartbeat(id); err != nil {
			t.Fatal(err)
		}
		if reaped := reg.sweepExpired(); len(reaped) != 0 {
			t.Fatalf("heartbeating app reaped: %v", reaped)
		}
		if code := askCode(t, mux, "domain=shop.example.com"); code != http.StatusOK {
			t.Fatalf("alive-but-unroutable ask: want 200, got %d", code)
		}
	}
}
