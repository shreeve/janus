package janus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

func TestFilesPrecompressedCaddyfile(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   []string
	}{
		{"janus {\n files {\n  precompressed\n }\n}", []string{"br", "zstd", "gzip"}},
		{"janus {\n files on {\n  precompressed gzip br\n }\n}", []string{"gzip", "br"}},
	} {
		app := new(App)
		if err := app.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.source)); err != nil {
			t.Fatalf("global rejected %q: %v", tc.source, err)
		}
		if app.Files == nil || !*app.Files || !slices.Equal(app.FilesPrecompressed, tc.want) {
			t.Fatalf("global %q: files=%v precompressed=%v", tc.source, app.Files, app.FilesPrecompressed)
		}
	}
	for _, source := range []string{
		"janus {\n files off {\n  precompressed\n }\n}",
		"janus {\n files {\n  precompressed br br\n }\n}",
		"janus {\n files {\n  precompressed bz\n }\n}",
		"janus {\n files {\n  precompressed\n  precompressed gzip\n }\n}",
		"janus {\n files {\n  unknown\n }\n}",
		"janus {\n files {\n  precompressed {\n   gzip\n  }\n }\n}",
	} {
		if err := new(App).UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("global accepted %q", source)
		}
	}
	for _, source := range []string{
		"janus {\n files {\n  precompressed\n }\n}",
		"janus {\n files on {\n  precompressed gzip\n }\n}",
	} {
		var handler Handler
		if err := handler.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("site accepted process-wide settings %q", source)
		}
	}
}

func TestAcceptedFileEncodings(t *testing.T) {
	configured := []string{"br", "zstd", "gzip"}
	for _, tc := range []struct {
		header string
		want   []string
	}{
		{"", nil},
		{"gzip;q=0.9, br;q=0.4", []string{"gzip", "br"}},
		{"gzip, zstd, br", []string{"br", "zstd", "gzip"}},
		{"br;q=0, *;q=0.8", []string{"zstd", "gzip"}},
		{"identity;q=1, br;q=0.5", []string{"identity", "br"}},
		{"identity;q=0.2, br;q=0.8", []string{"br", "identity"}},
		{"br;q=0, zstd;q=0, gzip;q=0, *;q=0", nil},
		{"deflate;q=1, *;q=0.5, gzip;q=0", []string{"br", "zstd"}},
	} {
		req := httptest.NewRequest(http.MethodGet, "http://app.test/file", nil)
		if tc.header != "" {
			req.Header.Set("Accept-Encoding", tc.header)
		}
		if got := acceptedFileEncodings(req, configured); !slices.Equal(got, tc.want) {
			t.Errorf("%q: got %v want %v", tc.header, got, tc.want)
		}
	}
}

func TestFilesPrecompressedJSONValidation(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name    string
		files   *bool
		formats []string
		wantErr bool
	}{
		{"unset", nil, nil, false},
		{"valid", &on, []string{"br", "zstd", "gzip"}, false},
		{"files absent", nil, []string{"br"}, true},
		{"files off", &off, []string{"br"}, true},
		{"unknown", &on, []string{"bz"}, true},
		{"duplicate", &on, []string{"gzip", "gzip"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFilesPrecompressed(tc.files, tc.formats)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantErr=%v", err, tc.wantErr)
			}
		})
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
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"ola.example.com": "ola"}},
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"alias.test": "common"}},
		{Host: "{site}.example.com", Dir: dir, Aliases: map[string]string{"alias.test": "Bad"}},
	}
	for i, site := range cases[1:] {
		if _, _, err := normalizeSitePolicy(site); err == nil {
			t.Errorf("bad site %d accepted: %+v", i, site)
		}
	}
}

func TestSitePolicyRemapAliasBeneathPattern(t *testing.T) {
	dir := t.TempDir()
	site, suffix, err := normalizeSitePolicy(&SitePolicy{
		Host:    "{site}.Example.com",
		Dir:     dir,
		Aliases: map[string]string{"Local.example.com": "ola"},
	})
	if err != nil {
		t.Fatalf("remap alias beneath pattern rejected: %v", err)
	}
	if suffix != "example.com" || site.Aliases["local.example.com"] != "ola" {
		t.Fatalf("normalized: suffix=%q aliases=%v", suffix, site.Aliases)
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

func TestServeFilesPrecompressedRepresentations(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	shellDir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(first, "bundle.json")
	write(bundle, "identity-bytes")
	write(bundle+".br", "brotli-bytes")
	write(bundle+".zst", "zstd-bytes")
	write(bundle+".gz", "gzip-bytes")
	write(filepath.Join(first, "plain.json"), "plain-identity")
	if err := os.Mkdir(filepath.Join(first, "plain.json.br"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(first, "priority.json"), "first-identity")
	write(filepath.Join(first, "orphan.json.br"), "orphan-sidecar")
	write(filepath.Join(second, "priority.json"), "second-identity")
	write(filepath.Join(second, "priority.json.br"), "second-brotli")
	write(filepath.Join(first, "docs", "index.html"), "index-identity")
	write(filepath.Join(first, "docs", "index.html.br"), "index-brotli")
	shell := filepath.Join(shellDir, "index.html")
	write(shell, "shell-identity")
	write(shell+".br", "shell-brotli")

	canonicalTime := time.Date(2026, time.August, 5, 1, 0, 0, 0, time.UTC)
	brotliTime := canonicalTime.Add(time.Hour)
	if err := os.Chtimes(bundle, canonicalTime, canonicalTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bundle+".br", brotliTime, brotliTime); err != nil {
		t.Fatal(err)
	}

	on := true
	h := &Handler{
		app:           &App{Files: &on, FilesPrecompressed: []string{"br", "zstd", "gzip"}},
		browseEnabled: true,
		browseCfg:     &BrowseSettings{},
	}
	rec := AppRecord{Files: &FilesPolicy{
		Roots: []FilesRoot{
			{Path: first, Cache: filesCacheNever, Browse: true},
			{Path: second, Cache: filesCacheRevalidate},
		},
		Shell: shell,
	}}
	serve := func(method, target, accept, encoding string, headers http.Header) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://app.test"+target, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if encoding != "" {
			req.Header.Set("Accept-Encoding", encoding)
		}
		for name, values := range headers {
			req.Header[name] = values
		}
		out := httptest.NewRecorder()
		handled, err := h.serveFiles(out, req, rec)
		if err != nil || !handled {
			t.Fatalf("%s %s: handled=%v err=%v", method, target, handled, err)
		}
		return out
	}

	br := serve(http.MethodGet, "/bundle.json", "", "br", nil)
	if br.Code != http.StatusOK || br.Body.String() != "brotli-bytes" {
		t.Fatalf("br response: code=%d body=%q", br.Code, br.Body.String())
	}
	if br.Header().Get("Content-Encoding") != "br" || br.Header().Get("Content-Type") != "application/json" ||
		br.Header().Get("Vary") != "Accept-Encoding" || br.Header().Get("Content-Length") != "12" ||
		br.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("br headers: %v", br.Header())
	}
	brETag := br.Header().Get("ETag")
	if !strings.Contains(brETag, "-br\"") || br.Header().Get("Last-Modified") != brotliTime.Format(http.TimeFormat) {
		t.Fatalf("br validators: etag=%q modified=%q", brETag, br.Header().Get("Last-Modified"))
	}

	identity := serve(http.MethodGet, "/bundle.json", "", "", nil)
	if identity.Body.String() != "identity-bytes" || identity.Header().Get("Content-Encoding") != "" ||
		identity.Header().Get("ETag") == brETag || identity.Header().Get("Vary") != "Accept-Encoding" ||
		identity.Header().Get("Last-Modified") != canonicalTime.Format(http.TimeFormat) {
		t.Fatalf("identity response: body=%q headers=%v", identity.Body.String(), identity.Header())
	}

	head := serve(http.MethodHead, "/bundle.json", "", "br", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "12" ||
		head.Header().Get("ETag") != brETag {
		t.Fatalf("HEAD response: code=%d body=%q headers=%v", head.Code, head.Body.String(), head.Header())
	}
	ranged := serve(http.MethodGet, "/bundle.json", "", "br", http.Header{"Range": {"bytes=1-3"}})
	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != "rot" ||
		ranged.Header().Get("Content-Encoding") != "br" || ranged.Header().Get("Content-Length") != "3" ||
		ranged.Header().Get("Content-Range") != "bytes 1-3/12" {
		t.Fatalf("range response: code=%d body=%q headers=%v", ranged.Code, ranged.Body.String(), ranged.Header())
	}

	if out := serve(http.MethodGet, "/bundle.json", "", "gzip;q=0.9, br;q=0.4", nil); out.Body.String() != "gzip-bytes" || out.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("q ordering: body=%q headers=%v", out.Body.String(), out.Header())
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "br;q=0, *;q=0.8", nil); out.Body.String() != "zstd-bytes" || out.Header().Get("Content-Encoding") != "zstd" {
		t.Fatalf("wildcard exclusion: body=%q headers=%v", out.Body.String(), out.Header())
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "identity;q=1, br;q=0.5", nil); out.Body.String() != "identity-bytes" || out.Header().Get("Content-Encoding") != "" {
		t.Fatalf("identity preference: body=%q headers=%v", out.Body.String(), out.Header())
	}

	if out := serve(http.MethodGet, "/bundle.json", "", "br", http.Header{"If-None-Match": {brETag}}); out.Code != http.StatusNotModified {
		t.Fatalf("br If-None-Match: code=%d headers=%v", out.Code, out.Header())
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "", http.Header{"If-None-Match": {brETag}}); out.Code != http.StatusOK {
		t.Fatalf("br etag validated identity: code=%d", out.Code)
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "gzip", http.Header{"If-None-Match": {brETag}}); out.Code != http.StatusOK {
		t.Fatalf("br etag validated gzip: code=%d", out.Code)
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "br", http.Header{"If-Modified-Since": {brotliTime.Format(http.TimeFormat)}}); out.Code != http.StatusNotModified {
		t.Fatalf("br If-Modified-Since: code=%d headers=%v", out.Code, out.Header())
	}

	if out := serve(http.MethodGet, "/plain.json", "", "br", nil); out.Body.String() != "plain-identity" || out.Header().Get("Content-Encoding") != "" {
		t.Fatalf("non-regular sidecar fallback: body=%q headers=%v", out.Body.String(), out.Header())
	}
	if out := serve(http.MethodGet, "/priority.json", "", "br", nil); out.Body.String() != "first-identity" || out.Header().Get("Content-Encoding") != "" {
		t.Fatalf("cross-root sidecar leak: body=%q headers=%v", out.Body.String(), out.Header())
	}
	if out := serve(http.MethodGet, "/orphan.json", "", "br", nil); out.Code != http.StatusNotFound {
		t.Fatalf("orphan sidecar created URL: code=%d body=%q", out.Code, out.Body.String())
	}
	if out := serve(http.MethodGet, "/docs/", "", "br", nil); out.Body.String() != "index-brotli" || out.Header().Get("Content-Encoding") != "br" || out.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("index sidecar: body=%q headers=%v", out.Body.String(), out.Header())
	}
	if out := serve(http.MethodGet, "/route", "text/html", "br", nil); out.Body.String() != "shell-brotli" || out.Header().Get("Content-Encoding") != "br" || out.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("shell sidecar: body=%q headers=%v", out.Body.String(), out.Header())
	}

	if err := os.Remove(bundle + ".br"); err != nil {
		t.Fatal(err)
	}
	if out := serve(http.MethodGet, "/bundle.json", "", "br", nil); out.Body.String() != "identity-bytes" || out.Header().Get("Content-Encoding") != "" {
		t.Fatalf("removed sidecar fallback: body=%q headers=%v", out.Body.String(), out.Header())
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
	if err := os.WriteFile(filepath.Join(first, "first.txt.br"), []byte("brotli"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "second.txt"), []byte("second"), 0o644); err != nil {
		b.Fatal(err)
	}
	shell := filepath.Join(b.TempDir(), "index.html")
	if err := os.WriteFile(shell, []byte("<main>shell</main>"), 0o644); err != nil {
		b.Fatal(err)
	}
	on := true
	h := &Handler{app: &App{Files: &on, FilesPrecompressed: []string{"br", "zstd", "gzip"}}}
	rec := AppRecord{Files: &FilesPolicy{Roots: []FilesRoot{
		{Path: first, Cache: filesCacheRevalidate},
		{Path: second, Cache: filesCacheRevalidate},
	}, Shell: shell}}
	for _, bench := range []struct {
		name   string
		target string
		accept string
		coding string
	}{
		{"first-root-hit", "/first.txt", "", ""},
		{"precompressed-br-hit", "/first.txt", "", "br"},
		{"precompressed-fallback", "/second.txt", "", "br"},
		{"second-root-hit", "/second.txt", "", ""},
		{"shell-fallback", "/route", "text/html", ""},
		{"static-miss", "/missing.bin", "", ""},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequest(http.MethodGet, "http://app.test"+bench.target, nil)
				if bench.accept != "" {
					req.Header.Set("Accept", bench.accept)
				}
				if bench.coding != "" {
					req.Header.Set("Accept-Encoding", bench.coding)
				}
				out := httptest.NewRecorder()
				if _, err := h.serveFiles(out, req, rec); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
