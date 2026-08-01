package janus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestFilesCaddyfileFormsAndCascade(t *testing.T) {
	for _, source := range []string{
		"janus {\n files\n}",
		"janus {\n files on\n}",
		"janus {\n files off\n}",
	} {
		app := new(App)
		if err := app.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err != nil {
			t.Fatalf("global rejected %q: %v", source, err)
		}
		var handler Handler
		if err := handler.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err != nil {
			t.Fatalf("site rejected %q: %v", source, err)
		}
	}
	for _, source := range []string{
		"janus {\n files maybe\n}",
		"janus {\n files on off\n}",
		"janus {\n files {\n root /tmp\n }\n}",
		"janus {\n files\n files off\n}",
	} {
		if err := new(App).UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("global accepted %q", source)
		}
		var handler Handler
		if err := handler.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("site accepted %q", source)
		}
	}
	on, off := true, false
	if (&Handler{app: &App{Files: &on}}).filesEnabled() != true {
		t.Fatal("site must inherit global files on")
	}
	if (&Handler{Files: &off, app: &App{Files: &on}}).filesEnabled() != false {
		t.Fatal("site files off must beat global on")
	}
	if (&Handler{}).filesEnabled() {
		t.Fatal("built-in files default must be off")
	}
}

func TestFilesPolicyValidation(t *testing.T) {
	good := &FilesPolicy{
		Roots: []FilesRoot{
			{Path: "/srv/sites/{site}/public", Cache: filesCacheNever},
			{Path: "/srv/common", Cache: filesCacheRevalidate},
		},
		ProxyFirst: []string{"/api", "/admin"},
		Shell:      "/srv/app/index.html",
	}
	if _, err := normalizeFilesPolicy(good, true); err != nil {
		t.Fatal(err)
	}
	bad := []*FilesPolicy{
		{Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/x", Cache: filesCacheRevalidate}}},
		{Roots: []FilesRoot{{Path: "relative", Cache: filesCacheRevalidate}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a//b", Cache: filesCacheRevalidate}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a/../b", Cache: filesCacheRevalidate}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a", Cache: filesCacheRevalidate}, {Path: "/a", Cache: filesCacheNever}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a/{other}", Cache: filesCacheRevalidate}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a", Cache: "sometimes"}}, Shell: "/x"},
		{Roots: []FilesRoot{{Path: "/a", Cache: filesCacheRevalidate}}, Shell: "/x/{site}"},
		{Roots: []FilesRoot{{Path: "/a", Cache: filesCacheRevalidate}}, Shell: "/x", ProxyFirst: []string{"/api", "/api/v1"}},
		{Roots: []FilesRoot{{Path: "/a", Cache: filesCacheRevalidate}}, Shell: "/x", ProxyFirst: []string{"/api/"}},
	}
	for i, policy := range bad {
		if _, err := normalizeFilesPolicy(policy, true); err == nil {
			t.Errorf("bad policy %d accepted: %+v", i, policy)
		}
	}
	if _, err := normalizeFilesPolicy(&FilesPolicy{Roots: []FilesRoot{{Path: "/x/{site}", Cache: filesCacheNever}}, Shell: "/x"}, false); err == nil {
		t.Fatal("{site} root without site accepted")
	}
}

func TestSiteRegistryResolutionAliasesConflictsAndLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ola"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := newAppRegistry()
	site := &SitePolicy{
		Host: "{site}.medlabs.health",
		Dir:  dir,
		Aliases: map[string]string{
			"localhost": "ola",
		},
	}
	rec, err := reg.createWithPolicy("medlabs", nil, site, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"ola.medlabs.health", "localhost"} {
		resolved, ok := reg.resolveRequestHost(host)
		if !ok || resolved.ID != rec.ID || resolved.siteValue != "ola" {
			t.Fatalf("resolve %q: %+v ok=%v", host, resolved, ok)
		}
	}
	if _, ok := reg.resolveHost("missing.medlabs.health"); ok {
		t.Fatal("missing direct child admitted")
	}
	if err := os.Symlink(filepath.Join(dir, "ola"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.resolveHost("link.medlabs.health"); ok {
		t.Fatal("symlink direct child admitted")
	}
	if _, ok := reg.resolveRequestHost("ola.medlabs.health:8443"); !ok {
		t.Fatal("valid port prevented pattern resolution")
	}
	for _, authority := range []string{"ola.medlabs.health:", "ola.medlabs.health:notaport", "127.0.0.1"} {
		if _, ok := reg.resolveRequestHost(authority); ok {
			t.Errorf("malformed or IP authority %q matched pattern", authority)
		}
	}
	if _, err := reg.create("exact", []string{"x.medlabs.health"}, ""); err == nil {
		t.Fatal("exact host beneath owned suffix did not conflict")
	}
	if _, err := reg.create("alias", []string{"localhost"}, ""); err == nil {
		t.Fatal("owned alias did not conflict")
	}
	if _, err := reg.createWithPolicy("nested", nil, &SitePolicy{
		Host: "{site}.team.medlabs.health",
		Dir:  dir,
	}, nil, ""); err == nil {
		t.Fatal("overlapping nested site suffix did not conflict")
	}
	hosts := []string{"other.test"}
	if _, err := reg.patch(rec.ID, nil, &hosts, nil); err == nil {
		t.Fatal("site-pattern app accepted hosts PATCH")
	}
	if err := reg.delete(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.create("exact", []string{"x.medlabs.health", "localhost"}, ""); err != nil {
		t.Fatalf("DELETE did not release suffix and alias claims: %v", err)
	}
}

func TestSitePolicyHardErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []*SitePolicy{
		nil,
		{Host: "example.com", Dir: dir},
		{Host: "x.{site}.example.com", Dir: dir},
		{Host: "{site}.{site}.example.com", Dir: dir},
		{Host: "{site}.bad_host", Dir: dir},
		{Host: "{site}.example.com", Dir: "relative"},
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"a.example.com": "ola"}},
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"alias.test": "common"}},
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"alias.test": "Bad"}},
	}
	for i, site := range cases[1:] {
		if _, _, err := normalizeSitePolicy(site); err == nil {
			t.Errorf("bad site %d accepted: %+v", i, site)
		}
	}
}

func TestFilesHTTPStrictPresenceAndNestedFields(t *testing.T) {
	mux := newTestControlMux(t)
	for _, body := range []string{
		`{"name":"x","hosts":null}`,
		`{"name":"x","site":null}`,
		`{"name":"x","hosts":["x.test"],"site":{"host":"{site}.x.test","dir":"/tmp"}}`,
		`{"name":"x","site":{"host":"{site}.x.test","dir":"/tmp","extra":1}}`,
		`{"name":"x","hosts":["x.test"],"files":null}`,
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","class":"x"}],"shell":"/tmp/index"}}`,
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","cache":"revalidate"}],"shell":"/tmp/index","extra":1}}`,
	} {
		code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps", body)
		if code != http.StatusBadRequest {
			t.Errorf("body %s: want 400, got %d", body, code)
		}
	}
}

func TestServeFilesOrderShellValidatorsHeadAndRange(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "same.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "source.rip"), []byte("value = 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "same.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "other.txt"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "styles.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	forever := t.TempDir()
	if err := os.WriteFile(filepath.Join(forever, "asset.js"), []byte("export{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(shell, []byte("<main>shell</main>"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := AppRecord{Files: &FilesPolicy{
		Roots: []FilesRoot{
			{Path: first, Cache: filesCacheNever},
			{Path: second, Cache: filesCacheRevalidate},
			{Path: forever, Cache: filesCacheForever},
		},
		ProxyFirst: []string{"/api"},
		Shell:      shell,
	}}
	h := new(Handler)
	serve := func(method, target, accept string, headers http.Header) (*httptest.ResponseRecorder, bool) {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Accept", accept)
		for name, values := range headers {
			req.Header[name] = values
		}
		out := httptest.NewRecorder()
		handled, err := h.serveFiles(out, req, rec)
		if err != nil {
			t.Fatal(err)
		}
		return out, handled
	}

	out, handled := serve(http.MethodGet, "http://app.test/same.txt", "", nil)
	if !handled || out.Body.String() != "first" {
		t.Fatalf("ordered root: handled=%v body=%q", handled, out.Body.String())
	}
	etag := out.Header().Get("ETag")
	if !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("weak ETag missing: %q", etag)
	}
	out, handled = serve(http.MethodHead, "http://app.test/other.txt", "", nil)
	if !handled || out.Body.Len() != 0 || out.Header().Get("Content-Length") != "5" {
		t.Fatalf("HEAD: handled=%v len=%d headers=%v", handled, out.Body.Len(), out.Header())
	}
	out, handled = serve(http.MethodGet, "http://app.test/other.txt", "", http.Header{"Range": {"bytes=1-3"}})
	if !handled || out.Code != http.StatusPartialContent || out.Body.String() != "the" {
		t.Fatalf("range: handled=%v code=%d body=%q", handled, out.Code, out.Body.String())
	}
	out, handled = serve(http.MethodGet, "http://app.test/missing", "text/html", nil)
	if !handled || out.Body.String() != "<main>shell</main>" {
		t.Fatalf("shell: handled=%v body=%q", handled, out.Body.String())
	}
	if got := out.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("shell Cache-Control=%q", got)
	}
	out, _ = serve(http.MethodGet, "http://app.test/source.rip", "", nil)
	if got := out.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("never Cache-Control=%q", got)
	}
	if got := out.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("rip Content-Type=%q", got)
	}
	out, _ = serve(http.MethodGet, "http://app.test/styles.css", "", nil)
	if got := out.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("revalidate Cache-Control=%q", got)
	}
	if got := out.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Fatalf("css Content-Type=%q", got)
	}
	out, _ = serve(http.MethodGet, "http://app.test/asset.js", "", nil)
	if got := out.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("forever Cache-Control=%q", got)
	}
	out, handled = serve(http.MethodGet, "http://app.test/missing.bin", "", nil)
	if !handled || out.Code != http.StatusNotFound {
		t.Fatalf("missing asset must be terminal 404: handled=%v code=%d", handled, out.Code)
	}
	if _, handled = serve(http.MethodGet, "http://app.test/api", "text/html", nil); handled {
		t.Fatal("proxy_first path served shell")
	}
	if _, handled = serve(http.MethodGet, "http://app.test/apian", "text/html", nil); !handled {
		t.Fatal("segment-boundary mismatch did not serve shell")
	}
	out, handled = serve(http.MethodPost, "http://app.test/missing", "text/html", nil)
	if !handled || out.Code != http.StatusNotFound {
		t.Fatalf("POST outside proxy_first must be terminal 404: handled=%v code=%d", handled, out.Code)
	}
}

func TestServeFilesRejectsUnsafeRequestPaths(t *testing.T) {
	h := new(Handler)
	rec := AppRecord{Files: &FilesPolicy{Roots: []FilesRoot{{Path: t.TempDir(), Cache: filesCacheRevalidate}}, Shell: filepath.Join(t.TempDir(), "index")}}
	for _, target := range []string{
		"http://app.test/a%2fb",
		"http://app.test/a%5cb",
		"http://app.test/a/../b",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		out := httptest.NewRecorder()
		handled, err := h.serveFiles(out, req, rec)
		if err != nil || !handled || out.Code != http.StatusBadRequest {
			t.Errorf("%s: handled=%v code=%d err=%v", target, handled, out.Code, err)
		}
	}
}

func TestHandlerRejectsUnsafePathsWithFilesOnOrOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		dp, reg := newTestDataPlane(t)
		if _, err := reg.create("app", []string{"app.test"}, ""); err != nil {
			t.Fatal(err)
		}
		h := &Handler{dp: dp, Files: &enabled}
		req := httptest.NewRequest(http.MethodGet, "http://app.test/a%2fb", nil)
		out := httptest.NewRecorder()
		err := h.ServeHTTP(out, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			t.Fatal("unsafe path reached next handler")
			return nil
		}))
		if err != nil || out.Code != http.StatusBadRequest {
			t.Fatalf("files=%v: code=%d err=%v", enabled, out.Code, err)
		}
	}
}

func TestFilesMissingAssetNeverRingsDoorbell(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	var rings atomic.Int32
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rings.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	rec, err := reg.createWithPolicy("app", []string{"app.test"}, nil, &FilesPolicy{
		Roots: []FilesRoot{{Path: t.TempDir(), Cache: filesCacheRevalidate}},
		Shell: filepath.Join(t.TempDir(), "missing-shell.html"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.setUpstreams(rec.ID, []Upstream{{Path: bell, Doorbell: true}}); err != nil {
		t.Fatal(err)
	}
	on := true
	h := &Handler{dp: dp, Files: &on}
	req := httptest.NewRequest(http.MethodGet, "http://app.test/missing.png", nil)
	out := httptest.NewRecorder()
	if err := h.ServeHTTP(out, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("missing asset reached next handler")
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if out.Code != http.StatusNotFound || rings.Load() != 0 {
		t.Fatalf("missing asset: code=%d rings=%d", out.Code, rings.Load())
	}
}

func TestHostPatchBumpsGenerationAndPurges(t *testing.T) {
	reg := newAppRegistry()
	rec, err := reg.create("app", []string{"a.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	purged := 0
	reg.setPurge(func(string) { purged++ })
	before := rec.gen.Load()
	hosts := []string{"b.test"}
	if _, err := reg.patch(rec.ID, nil, &hosts, nil); err != nil {
		t.Fatal(err)
	}
	if rec.gen.Load() != before+1 || purged != 1 {
		t.Fatalf("generation=%d want %d, purges=%d", rec.gen.Load(), before+1, purged)
	}
}

func TestSiteClaimsReleaseOnHeartbeatReapAndClonesAreDeep(t *testing.T) {
	reg, clock := newClockedRegistry(t, 10*time.Second)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ola"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec, err := reg.createWithPolicy("sites", nil, &SitePolicy{
		Host:    "{site}.example.test",
		Dir:     dir,
		Aliases: map[string]string{"local.test": "ola"},
	}, &FilesPolicy{
		Roots:      []FilesRoot{{Path: "/srv/{site}", Cache: filesCacheNever}},
		ProxyFirst: []string{"/api"},
		Shell:      "/srv/index.html",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reg.get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Site.Aliases["local.test"] = "changed"
	got.Files.Roots[0].Path = "/changed"
	got.Files.ProxyFirst[0] = "/changed"
	again, _ := reg.get(rec.ID)
	if again.Site.Aliases["local.test"] != "ola" || again.Files.Roots[0].Path != "/srv/{site}" ||
		again.Files.ProxyFirst[0] != "/api" {
		t.Fatal("get returned registry-owned nested storage")
	}
	clock.advance(11 * time.Second)
	if reaped := reg.sweepExpired(); len(reaped) != 1 {
		t.Fatalf("reaped=%v", reaped)
	}
	if _, err := reg.create("exact", []string{"x.example.test", "local.test"}, ""); err != nil {
		t.Fatalf("reap did not release site claims: %v", err)
	}
}

func TestHandlerStripsAndInjectsTrustedRipSite(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	seen := make(chan string, 2)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get(ripSiteHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ola"), 0o755); err != nil {
		t.Fatal(err)
	}
	pattern, err := reg.createWithPolicy("sites", nil, &SitePolicy{
		Host: "{site}.example.test",
		Dir:  dir,
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.setUpstreams(pattern.ID, []Upstream{{Path: sock}}); err != nil {
		t.Fatal(err)
	}
	exact, err := reg.create("exact", []string{"exact.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.setUpstreams(exact.ID, []Upstream{{Path: sock}}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{
		dp: dp,
		cacheCfg: &cacheSite{
			store:   newCacheStore(defaultCacheMaxBytes, defaultCacheAppShare),
			ttl:     defaultCacheTTL,
			ttlMax:  defaultCacheTTLMax,
			maxBody: defaultCacheMaxBody,
		},
	}
	next := caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("unexpected next handler")
		return nil
	})
	for _, host := range []string{"ola.example.test", "exact.test"} {
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Header.Set(ripSiteHeader, "attacker")
		out := httptest.NewRecorder()
		if err := handler.ServeHTTP(out, req, next); err != nil {
			t.Fatal(err)
		}
		want := ""
		if host == "ola.example.test" {
			want = "ola"
		}
		if got := <-seen; got != want {
			t.Fatalf("%s worker Rip-Site=%q, want %q", host, got, want)
		}
	}
	snapshot, ok := hubHeaderSnapshot(http.Header{ripSiteHeader: {"ola"}})
	if !ok || len(snapshot.Values(ripSiteHeader)) != 1 || snapshot.Get(ripSiteHeader) != "ola" {
		t.Fatalf("hub snapshot lost trusted Rip-Site: %v", snapshot)
	}
}

func BenchmarkServeFiles(b *testing.B) {
	first := b.TempDir()
	second := b.TempDir()
	if err := os.WriteFile(filepath.Join(first, "first.txt"), []byte("first"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "second.txt"), []byte("second"), 0o644); err != nil {
		b.Fatal(err)
	}
	shell := filepath.Join(b.TempDir(), "index.html")
	if err := os.WriteFile(shell, []byte("<main>shell</main>"), 0o644); err != nil {
		b.Fatal(err)
	}
	h := new(Handler)
	rec := AppRecord{Files: &FilesPolicy{Roots: []FilesRoot{
		{Path: first, Cache: filesCacheRevalidate},
		{Path: second, Cache: filesCacheRevalidate},
	}, Shell: shell}}
	for _, bench := range []struct {
		name   string
		target string
		accept string
	}{
		{"first-root-hit", "/first.txt", ""},
		{"second-root-hit", "/second.txt", ""},
		{"shell-fallback", "/route", "text/html"},
		{"static-miss", "/missing.bin", ""},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "http://app.test"+bench.target, nil)
				if bench.accept != "" {
					req.Header.Set("Accept", bench.accept)
				}
				out := httptest.NewRecorder()
				if _, err := h.serveFiles(out, req, rec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
