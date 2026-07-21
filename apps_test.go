package janus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

func TestMintIDSuffix(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9]{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := mintIDSuffix()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(s) {
			t.Fatalf("suffix %q does not match [a-z0-9]{6}", s)
		}
		seen[s] = true
	}
	if len(seen) < 90 {
		t.Fatalf("suffixes are suspiciously non-random: %d unique of 100", len(seen))
	}
}

func TestRegistryCreateMintsPrefixedID(t *testing.T) {
	r := newAppRegistry()
	rec, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^shop-[a-z0-9]{6}$`).MatchString(rec.ID) {
		t.Fatalf("id %q does not match name-xxxxxx", rec.ID)
	}
	if rec.Name != "shop" || len(rec.Hosts) != 1 || rec.Hosts[0] != "shop.example.com" {
		t.Fatalf("record: %+v", rec)
	}
	if rec.Upstreams == nil || len(rec.Upstreams) != 0 {
		t.Fatalf("new app upstreams: %+v", rec.Upstreams)
	}
}

func TestRegistryCreateValidation(t *testing.T) {
	r := newAppRegistry()
	cases := []struct {
		name  string
		hosts []string
	}{
		{"", []string{"a.example.com"}},              // name required
		{"Shop", []string{"a.example.com"}},          // uppercase name
		{"-shop", []string{"a.example.com"}},         // leading hyphen
		{"shop", nil},                                // hosts required
		{"shop", []string{}},                         // hosts non-empty
		{"shop", []string{""}},                       // empty host
		{"shop", []string{"has space.com"}},          // not a hostname
		{"shop", []string{"bad_host.com"}},           // underscore
		{"shop", []string{"-bad.example.com"}},       // label leading hyphen
		{"shop", []string{"a.com", "a.com"}},         // duplicate in request
		{"shop", []string{strings.Repeat("x", 254)}}, // too long
	}
	for _, tt := range cases {
		_, err := r.create(tt.name, tt.hosts, "")
		var ae *apiError
		if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Fatalf("create(%q,%v): want 400, got %v", tt.name, tt.hosts, err)
		}
	}
}

func TestRegistryHostFirstWins(t *testing.T) {
	r := newAppRegistry()
	first, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.create("rival", []string{"rival.example.com", "shop.example.com"}, "")
	var ae *apiError
	if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("want 409, got %v", err)
	}
	if !strings.Contains(ae.Msg, "shop.example.com") || !strings.Contains(ae.Msg, first.ID) {
		t.Fatalf("conflict error must name host and holder: %q", ae.Msg)
	}
	// The failed create must not have claimed rival.example.com.
	if _, err := r.create("rival", []string{"rival.example.com"}, ""); err != nil {
		t.Fatalf("rival host leaked from failed create: %v", err)
	}
	// Hostnames compare case-insensitively.
	_, err = r.create("shout", []string{"SHOP.Example.COM"}, "")
	if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("case-insensitive conflict: want 409, got %v", err)
	}
}

func TestRegistryDeleteFreesHosts(t *testing.T) {
	r := newAppRegistry()
	rec, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.get(rec.ID); err == nil {
		t.Fatal("deleted app still fetchable")
	}
	if _, err := r.create("shop2", []string{"shop.example.com"}, ""); err != nil {
		t.Fatalf("host not freed by delete: %v", err)
	}
	var ae *apiError
	if err := r.delete(rec.ID); err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("double delete: want 404, got %v", err)
	}
}

func TestRegistryPatch(t *testing.T) {
	r := newAppRegistry()
	a, _ := r.create("shop", []string{"shop.example.com"}, "")
	b, _ := r.create("blog", []string{"blog.example.com"}, "")

	// Rename + swap hosts.
	name := "store"
	hosts := []string{"store.example.com"}
	rec, err := r.patch(a.ID, &name, &hosts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "store" || rec.Hosts[0] != "store.example.com" {
		t.Fatalf("patched: %+v", rec)
	}
	// Old host is freed.
	if _, err := r.create("other", []string{"shop.example.com"}, ""); err != nil {
		t.Fatalf("old host not freed by patch: %v", err)
	}
	// Conflict with another app's host → 409, and nothing changes.
	hosts = []string{"blog.example.com"}
	_, err = r.patch(a.ID, nil, &hosts, nil)
	var ae *apiError
	if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("want 409, got %v", err)
	}
	if !strings.Contains(ae.Msg, "blog.example.com") || !strings.Contains(ae.Msg, b.ID) {
		t.Fatalf("conflict error must name host and holder: %q", ae.Msg)
	}
	got, _ := r.get(a.ID)
	if got.Hosts[0] != "store.example.com" {
		t.Fatalf("failed patch mutated hosts: %+v", got.Hosts)
	}
	// Re-claiming a host the app already holds is fine.
	hosts = []string{"store.example.com", "store2.example.com"}
	if _, err := r.patch(a.ID, nil, &hosts, nil); err != nil {
		t.Fatalf("re-claim own host: %v", err)
	}
	// Empty patch → 400; unknown id → 404.
	if _, err := r.patch(a.ID, nil, nil, nil); err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Fatalf("empty patch: want 400, got %v", err)
	}
	if _, err := r.patch("nope-000000", &name, nil, nil); err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %v", err)
	}
}

func TestValidateUpstreamsDoorbellSoleEntry(t *testing.T) {
	cases := []struct {
		ups []Upstream
		ok  bool
	}{
		{[]Upstream{}, true},                      // empty = not routable
		{[]Upstream{{Path: "/run/a.sock"}}, true}, // one worker
		{[]Upstream{{Path: "/run/a.sock"}, {Path: "/run/b.sock"}}, true},
		{[]Upstream{{Path: "/run/bell.sock", Doorbell: true}}, true},                                        // sole doorbell
		{[]Upstream{{Path: "/run/bell.sock", Doorbell: true}, {Path: "/run/a.sock"}}, false},                // mixed
		{[]Upstream{{Path: "/run/a.sock"}, {Path: "/run/bell.sock", Doorbell: true}}, false},                // mixed
		{[]Upstream{{Path: "/run/b1.sock", Doorbell: true}, {Path: "/run/b2.sock", Doorbell: true}}, false}, // two doorbells
		{[]Upstream{{Path: ""}}, false},                                                                     // empty path
		{[]Upstream{{Path: "/run/a.sock"}, {Path: "/run/a.sock"}}, false},                                   // duplicate path
	}
	for i, tt := range cases {
		err := validateUpstreams(tt.ups)
		if tt.ok && err != nil {
			t.Fatalf("case %d: unexpected error %v", i, err)
		}
		if !tt.ok {
			var ae *apiError
			if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusBadRequest {
				t.Fatalf("case %d: want 400, got %v", i, err)
			}
		}
	}
}

func TestRegistrySetUpstreamsAtomicSwap(t *testing.T) {
	r := newAppRegistry()
	rec, _ := r.create("shop", []string{"shop.example.com"}, "")

	if _, err := r.setUpstreams(rec.ID, []Upstream{{Path: "/run/a.sock"}, {Path: "/run/b.sock"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.get(rec.ID)
	if len(got.Upstreams) != 2 {
		t.Fatalf("upstreams: %+v", got.Upstreams)
	}
	// Full-list swap replaces, never merges.
	if _, err := r.setUpstreams(rec.ID, []Upstream{{Path: "/run/c.sock"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = r.get(rec.ID)
	if len(got.Upstreams) != 1 || got.Upstreams[0].Path != "/run/c.sock" {
		t.Fatalf("swap did not replace: %+v", got.Upstreams)
	}
	// Empty list is legal (= not routable).
	if _, err := r.setUpstreams(rec.ID, []Upstream{}); err != nil {
		t.Fatal(err)
	}
	got, _ = r.get(rec.ID)
	if len(got.Upstreams) != 0 {
		t.Fatalf("empty swap: %+v", got.Upstreams)
	}
	// Invalid list leaves the current list untouched.
	if _, err := r.setUpstreams(rec.ID, []Upstream{{Path: "/run/d.sock"}}); err != nil {
		t.Fatal(err)
	}
	_, err := r.setUpstreams(rec.ID, []Upstream{
		{Path: "/run/bell.sock", Doorbell: true},
		{Path: "/run/e.sock"},
	})
	if err == nil {
		t.Fatal("mixed doorbell list: want error")
	}
	got, _ = r.get(rec.ID)
	if len(got.Upstreams) != 1 || got.Upstreams[0].Path != "/run/d.sock" {
		t.Fatalf("failed swap mutated list: %+v", got.Upstreams)
	}
	// Unknown id → 404.
	var ae *apiError
	if _, err := r.setUpstreams("nope-000000", nil); err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("unknown id: want 404, got %v", err)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r := newAppRegistry()
	rec, _ := r.create("shop", []string{"shop.example.com"}, "")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if n%2 == 0 {
					_, _ = r.setUpstreams(rec.ID, []Upstream{{Path: "/run/x.sock"}})
				} else {
					_, _ = r.get(rec.ID)
					_ = r.list()
				}
			}
		}(i)
	}
	wg.Wait()
	got, err := r.get(rec.ID)
	if err != nil || len(got.Upstreams) != 1 {
		t.Fatalf("after concurrency: %+v err %v", got, err)
	}
}

// --- HTTP layer ------------------------------------------------------------

func newTestControlMux(t *testing.T) *http.ServeMux {
	t.Helper()
	app := &App{Control: []Control{{Mode: "local", Listen: DefaultControlLocal}}}
	if err := app.Control[0].normalize(); err != nil {
		t.Fatal(err)
	}
	app.appsReg = newAppRegistry()
	return app.controlMux()
}

func doJSON(t *testing.T, mux *http.ServeMux, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	var out map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr.Code, out
}

func TestAppsHTTPLifecycle(t *testing.T) {
	mux := newTestControlMux(t)

	// Register.
	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if !regexp.MustCompile(`^shop-[a-z0-9]{6}$`).MatchString(id) {
		t.Fatalf("id: %q", id)
	}

	// List.
	req := httptest.NewRequest(http.MethodGet, "/1.0/apps", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), id) {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}

	// Get.
	code, body = doJSON(t, mux, http.MethodGet, "/1.0/apps/"+id, "")
	if code != http.StatusOK || body["name"] != "shop" {
		t.Fatalf("get: %d %v", code, body)
	}

	// Unknown id → 404.
	code, _ = doJSON(t, mux, http.MethodGet, "/1.0/apps/shop-zzzzzz", "")
	if code != http.StatusNotFound {
		t.Fatalf("get unknown: %d", code)
	}

	// Host conflict → 409 naming host and holder.
	code, body = doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"rival","hosts":["shop.example.com"]}`)
	if code != http.StatusConflict {
		t.Fatalf("conflict: %d %v", code, body)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "shop.example.com") || !strings.Contains(msg, id) {
		t.Fatalf("conflict message: %q", msg)
	}

	// Bad create bodies → 400.
	for _, b := range []string{
		`not json`,
		`{"hosts":["a.example.com"]}`,
		`{"name":"shop2"}`,
		`{"name":"shop2","hosts":[]}`,
		`{"name":"shop2","hosts":["bad host"]}`,
	} {
		code, _ = doJSON(t, mux, http.MethodPost, "/1.0/apps", b)
		if code != http.StatusBadRequest {
			t.Fatalf("create %q: want 400, got %d", b, code)
		}
	}

	// PUT upstreams: good, empty, mixed doorbell rejected, malformed.
	code, body = doJSON(t, mux, http.MethodPut, "/1.0/apps/"+id+"/upstreams",
		`{"upstreams":[{"path":"/run/a.sock"},{"path":"/run/b.sock"}]}`)
	if code != http.StatusOK {
		t.Fatalf("put upstreams: %d %v", code, body)
	}
	code, body = doJSON(t, mux, http.MethodGet, "/1.0/apps/"+id, "")
	if code != http.StatusOK {
		t.Fatalf("get after put: %d", code)
	}
	if ups, _ := body["upstreams"].([]any); len(ups) != 2 {
		t.Fatalf("upstreams after put: %v", body)
	}
	code, _ = doJSON(t, mux, http.MethodPut, "/1.0/apps/"+id+"/upstreams",
		`{"upstreams":[]}`)
	if code != http.StatusOK {
		t.Fatalf("put empty upstreams: %d", code)
	}
	code, body = doJSON(t, mux, http.MethodPut, "/1.0/apps/"+id+"/upstreams",
		`{"upstreams":[{"path":"/run/bell.sock","doorbell":true},{"path":"/run/a.sock"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("mixed doorbell: want 400, got %d %v", code, body)
	}
	code, _ = doJSON(t, mux, http.MethodPut, "/1.0/apps/"+id+"/upstreams", `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("missing upstreams key: want 400, got %d", code)
	}
	code, _ = doJSON(t, mux, http.MethodPut, "/1.0/apps/shop-zzzzzz/upstreams",
		`{"upstreams":[]}`)
	if code != http.StatusNotFound {
		t.Fatalf("put unknown id: want 404, got %d", code)
	}

	// PATCH name.
	code, body = doJSON(t, mux, http.MethodPatch, "/1.0/apps/"+id,
		`{"name":"store"}`)
	if code != http.StatusOK || body["name"] != "store" {
		t.Fatalf("patch: %d %v", code, body)
	}

	// Delete → 204; get after delete → 404.
	code, _ = doJSON(t, mux, http.MethodDelete, "/1.0/apps/"+id, "")
	if code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	code, _ = doJSON(t, mux, http.MethodGet, "/1.0/apps/"+id, "")
	if code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", code)
	}
	code, _ = doJSON(t, mux, http.MethodDelete, "/1.0/apps/"+id, "")
	if code != http.StatusNotFound {
		t.Fatalf("delete unknown: %d", code)
	}
}

func asAPIError(err error, target **apiError) bool {
	ae, ok := err.(*apiError)
	if ok {
		*target = ae
	}
	return ok
}
