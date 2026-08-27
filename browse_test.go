package janus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func TestBrowseGlobalAndSiteGrammar(t *testing.T) {
	goodGlobal := []string{
		"janus {\n browse\n}",
		"janus {\n browse {\n }\n}",
		"janus {\n browse {\n timeout 2s\n max_output 1MB\n concurrency 3\n renderer .MD {\n command cat {file}\n content_type text/html\n }\n }\n}",
	}
	for _, source := range goodGlobal {
		app := new(App)
		if err := app.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err != nil {
			t.Errorf("global rejected %q: %v", source, err)
		}
	}
	sizeApp := new(App)
	if err := sizeApp.UnmarshalCaddyfile(caddyfile.NewTestDispenser("janus {\n browse {\n max_output 1MB\n }\n}")); err != nil {
		t.Fatal(err)
	}
	if sizeApp.Browse.MaxOutput == nil || *sizeApp.Browse.MaxOutput != 1<<20 {
		t.Fatalf("MB is not binary: %v", sizeApp.Browse.MaxOutput)
	}
	badGlobal := []string{
		"janus {\n browse on\n}",
		"janus {\n browse off\n}",
		"janus {\n browse\n browse\n}",
		"janus {\n browse { nope x }\n}",
		"janus {\n browse { timeout 0s }\n}",
		"janus {\n browse { max_output 0 }\n}",
		"janus {\n browse { max_output 9223372036854775807 }\n}",
		"janus {\n browse { concurrency 0 }\n}",
		"janus {\n browse { renderer md { command cat {file}\n content_type text/html } }\n}",
		"janus {\n browse { renderer .md { command cat\n content_type text/html } }\n}",
		"janus {\n browse { renderer .md { command {file}\n content_type text/html } }\n}",
		"janus {\n browse { renderer .md { command cat {file} {file}\n content_type text/html } }\n}",
		"janus {\n browse { renderer .md { command cat {file} } }\n}",
		"janus {\n browse { renderer .md { command cat {file}\n content_type bad/type;param } }\n}",
		"janus {\n browse { renderer .md {\n command cat {file}\n content_type text/html\n max_output 9223372036854775807\n }\n }\n}",
	}
	for _, source := range badGlobal {
		if err := new(App).UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("global accepted %q", source)
		}
	}
	maxOutput := int64(math.MaxInt64)
	if err := (&App{Browse: &BrowseSettings{MaxOutput: &maxOutput}}).provisionBrowse(); err == nil {
		t.Fatal("programmatic MaxInt64 max_output accepted")
	}

	goodSite := []string{
		"janus {\n browse\n}",
		"janus {\n browse on\n}",
		"janus {\n browse off\n}",
		"janus {\n browse { }\n}",
		"janus {\n browse on {\n root /tmp\n root /var/tmp forever\n }\n}",
	}
	for _, source := range goodSite {
		var handler Handler
		if err := handler.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err != nil {
			t.Errorf("site rejected %q: %v", source, err)
		}
	}
	badSite := []string{
		"janus {\n browse maybe\n}",
		"janus {\n browse off { root /tmp }\n}",
		"janus {\n browse { root relative }\n}",
		"janus {\n browse { root /tmp sometimes }\n}",
		"janus {\n browse { theme /tmp }\n}",
		"janus {\n browse\n browse\n}",
	}
	for _, source := range badSite {
		var handler Handler
		if err := handler.UnmarshalCaddyfile(caddyfile.NewTestDispenser(source)); err == nil {
			t.Errorf("site accepted %q", source)
		}
	}
}

func TestBrowseExtensionAndRendererMatching(t *testing.T) {
	for _, good := range []string{".md", ".TAR.GZ", "._x-1"} {
		if _, err := normalizeBrowseExtension(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{"", ".", "md", ".é", ".x/y", ".x y"} {
		if _, err := normalizeBrowseExtension(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	short := &browseRenderer{extension: ".gz"}
	long := &browseRenderer{extension: ".tar.gz"}
	settings := &BrowseSettings{renderers: map[string]*browseRenderer{".gz": short, ".tar.gz": long}}
	if got := settings.matchRenderer("FILE.TAR.GZ"); got != long {
		t.Fatalf("longest suffix: got %p want %p", got, long)
	}
	if got := settings.matchRenderer(".gz"); got != nil {
		t.Fatalf("suffix without basename matched: %v", got)
	}
}

func TestBrowseCascadeAndFilesConflict(t *testing.T) {
	off := false
	global := &App{Browse: &BrowseSettings{}}
	handler := &Handler{app: global}
	if err := handler.provisionBrowse(); err != nil || !handler.browseEnabled {
		t.Fatalf("global browse did not cascade: enabled=%v err=%v", handler.browseEnabled, err)
	}
	if !handler.filesEnabled() {
		t.Fatal("browse on with files unset did not make files effective on")
	}
	handler = &Handler{app: global, Browse: &BrowseSiteSettings{Enabled: &off}}
	if err := handler.provisionBrowse(); err != nil || handler.browseEnabled {
		t.Fatalf("site off did not beat global on: enabled=%v err=%v", handler.browseEnabled, err)
	}
	handler = &Handler{app: &App{Browse: &BrowseSettings{}, Files: &off}}
	if err := handler.provisionBrowse(); err == nil {
		t.Fatal("effective browse on accepted effective files off")
	}
	handler = &Handler{
		Browse: &BrowseSiteSettings{Enabled: &off, Roots: []BrowseRoot{{Path: "/tmp"}}},
	}
	if err := handler.provisionBrowse(); err == nil {
		t.Fatal("cold roots accepted effective browse off")
	}

	siteOnlyApp := &App{state: &janusState{browse: newBrowseSupervisor()}}
	if err := siteOnlyApp.provisionBrowse(); err != nil {
		t.Fatal(err)
	}
	if siteOnlyApp.Browse != nil {
		t.Fatal("internal browse runtime turned global cascade on")
	}
	on := true
	handler = &Handler{app: siteOnlyApp, Browse: &BrowseSiteSettings{Enabled: &on}}
	if err := handler.provisionBrowse(); err != nil || !handler.browseEnabled || handler.browseCfg == nil ||
		handler.browseCfg.theme == nil {
		t.Fatalf("site-only browse runtime: enabled=%v config=%v err=%v", handler.browseEnabled, handler.browseCfg, err)
	}
	root := t.TempDir()
	listing := httptest.NewRecorder()
	if !handler.serveBrowseRoots(listing, httptest.NewRequest(http.MethodGet, "http://site.test/", nil), "/",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false) || listing.Code != http.StatusOK {
		t.Fatalf("site-only browse listing: %d %s", listing.Code, listing.Body.String())
	}
	siteOnlyApp.browseSites = []browseSiteEntry{{enabled: true, handler: handler}}
	status := siteOnlyApp.browseStatus()
	if status["enabled"] != true || status["theme"] != "embedded" {
		t.Fatalf("site-only browse status: %v", status)
	}
	inheriting := &Handler{app: siteOnlyApp}
	if err := inheriting.provisionBrowse(); err != nil || inheriting.browseEnabled {
		t.Fatalf("internal runtime affected global cascade: enabled=%v err=%v", inheriting.browseEnabled, err)
	}
}

func TestBrowseEmbeddedAndCustomThemeValidation(t *testing.T) {
	embedded, err := loadBrowseTheme("")
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded.hash) != 24 || embedded.assets["browse.css"].etag == "" {
		t.Fatalf("embedded theme: hash=%q assets=%v", embedded.hash, embedded.assets)
	}
	dir := t.TempDir()
	for name, asset := range embedded.assets {
		if err := os.WriteFile(filepath.Join(dir, name), asset.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first, err := loadBrowseTheme(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadBrowseTheme(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.hash != second.hash {
		t.Fatalf("nondeterministic hash: %s != %s", first.hash, second.hash)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`{{if false}}{{.Missing}}{{end}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrowseTheme(dir); err == nil || !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("hidden unknown field accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`{{unknown .Title}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrowseTheme(dir); err == nil {
		t.Fatal("unknown function accepted")
	}

	invocationDir := t.TempDir()
	for name, asset := range embedded.assets {
		if err := os.WriteFile(filepath.Join(invocationDir, name), asset.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hiddenInvocation := `{{if false}}{{template "link" .Parent}}{{end}}{{define "link"}}{{.Size}}{{end}}`
	if err := os.WriteFile(filepath.Join(invocationDir, "index.html"), []byte(hiddenInvocation), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrowseTheme(invocationDir); err == nil || !strings.Contains(err.Error(), ".Size") {
		t.Fatalf("hidden bad template invocation accepted: %v", err)
	}
	badPipeline := `{{if false}}{{template "link" .Missing}}{{end}}{{define "link"}}ok{{end}}`
	if err := os.WriteFile(filepath.Join(invocationDir, "index.html"), []byte(badPipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrowseTheme(invocationDir); err == nil || !strings.Contains(err.Error(), ".Missing") {
		t.Fatalf("hidden bad template pipeline accepted: %v", err)
	}

	hugeDir := t.TempDir()
	for name, asset := range embedded.assets {
		if err := os.WriteFile(filepath.Join(hugeDir, name), asset.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Truncate(filepath.Join(hugeDir, "browse.css"), browseThemeMaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBrowseTheme(hugeDir); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Fatalf("single oversized theme asset accepted: %v", err)
	}
}

func TestBrowseThemePublicationRequiresSuccessfulRequest(t *testing.T) {
	supervisor := newBrowseSupervisor()
	app := &App{state: &janusState{browse: supervisor}}
	if err := app.provisionBrowse(); err != nil {
		t.Fatal(err)
	}
	hash := app.browseRuntime.theme.hash
	if supervisor.theme(hash) != nil {
		t.Fatal("provisional theme was published during provisioning")
	}

	h := &Handler{
		app: app, browseEnabled: true, browseCfg: app.browseRuntime,
		logger: zap.NewNop(),
	}
	target := browseAssetPrefix + hash + "/browse.css"
	out := httptest.NewRecorder()
	h.serveBrowseAsset(out, httptest.NewRequest(http.MethodGet, target, nil))
	if out.Code != http.StatusOK || supervisor.theme(hash) == nil {
		t.Fatalf("successfully used current theme was not retained: status=%d", out.Code)
	}

	customDir := t.TempDir()
	for name, asset := range app.browseRuntime.theme.assets {
		data := asset.data
		if name == "browse.css" {
			data = append(append([]byte(nil), data...), '\n')
		}
		if err := os.WriteFile(filepath.Join(customDir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	provisional := &App{
		Browse: &BrowseSettings{Theme: customDir},
		state:  &janusState{browse: supervisor},
	}
	if err := provisional.provisionBrowse(); err != nil {
		t.Fatal(err)
	}
	provisionalHash := provisional.browseRuntime.theme.hash
	if provisionalHash == hash || supervisor.theme(provisionalHash) != nil {
		t.Fatal("unused provisional theme became globally addressable")
	}
	nextHandler := &Handler{
		app: provisional, browseEnabled: true, browseCfg: provisional.browseRuntime,
		logger: zap.NewNop(),
	}
	oldAsset := httptest.NewRecorder()
	nextHandler.serveBrowseAsset(oldAsset, httptest.NewRequest(http.MethodGet, target, nil))
	if oldAsset.Code != http.StatusOK {
		t.Fatalf("retained old theme unavailable to next generation: %d", oldAsset.Code)
	}

	badDir := t.TempDir()
	bad := &App{
		Browse: &BrowseSettings{Theme: badDir},
		state:  &janusState{browse: supervisor},
	}
	if err := bad.provisionBrowse(); err == nil {
		t.Fatal("rejected theme provisioned")
	}
	if len(supervisor.themes) != 1 {
		t.Fatalf("rejected theme changed retained themes: %v", supervisor.themes)
	}
}

func newBrowseTestHandler(t testing.TB, renderers map[string]*browseRenderer) *Handler {
	t.Helper()
	theme, err := loadBrowseTheme("")
	if err != nil {
		t.Fatal(err)
	}
	supervisor := newBrowseSupervisor()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtime := &BrowseSettings{theme: theme, renderers: renderers}
	app := &App{
		Browse:        runtime,
		browseRuntime: runtime,
		browseCtx:     ctx,
		state:         &janusState{browse: supervisor},
	}
	return &Handler{app: app, browseEnabled: true, browseCfg: runtime, logger: zap.NewNop()}
}

func TestBrowseListingIndexRawAndCachePolicies(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".dot": "dot", "a b.md": "# hello", "image.png": "png", "audio.mp3": "mp3",
		"document.pdf": "pdf", "<script>.txt": "safe", "z.txt": "z",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	invalidName := string([]byte{0xff, '.', 't', 'x', 't'})
	invalidUTF8Created := os.WriteFile(filepath.Join(root, invalidName), []byte("invalid"), 0o644) == nil
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	renderer := &browseRenderer{extension: ".md", contentType: "text/html; charset=utf-8"}
	h := newBrowseTestHandler(t, map[string]*browseRenderer{".md": renderer})
	roots := []activeBrowseRoot{{path: root, cache: filesCacheRevalidate, browse: true}}

	request := httptest.NewRequest(http.MethodGet, "http://x/", nil)
	response := httptest.NewRecorder()
	if !h.serveBrowseRoots(response, request, "/", roots, "", false) {
		t.Fatal("listing not handled")
	}
	body := response.Body.String()
	wants := []string{".dot", "folder/", "a b.md", "a%20b.md?raw", "image.png", "#symlink",
		"&lt;script&gt;.txt", "%3Cscript%3E.txt"}
	if invalidUTF8Created {
		wants = append(wants, "%FF.txt", "�.txt")
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("listing missing %q:\n%s", want, body)
		}
	}
	if strings.Index(body, "folder/") > strings.Index(body, ".dot") {
		t.Fatalf("directory did not sort before files:\n%s", body)
	}
	escaped := httptest.NewRecorder()
	if h.serveBrowseRoots(escaped, httptest.NewRequest(http.MethodGet, "http://x/escape", nil), "/escape", roots, "", false) {
		t.Fatal("escaping symlink was served")
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("listing headers: %v", response.Header())
	}

	head := httptest.NewRecorder()
	if !h.serveBrowseRoots(head, httptest.NewRequest(http.MethodHead, "http://x/", nil), "/", roots, "", false) {
		t.Fatal("HEAD listing not handled")
	}
	if head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatalf("HEAD listing: len=%d headers=%v", head.Body.Len(), head.Header())
	}

	redirect := httptest.NewRecorder()
	h.serveBrowseRoots(redirect, httptest.NewRequest(http.MethodGet, "http://x/folder?q=1", nil), "/folder", roots, "", false)
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "/folder/?q=1" {
		t.Fatalf("directory redirect: %d %q", redirect.Code, redirect.Header().Get("Location"))
	}

	indexBytes := []byte{0, 1, 2, 3}
	if err := os.WriteFile(filepath.Join(root, "folder", "index.rip"), indexBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	index := httptest.NewRecorder()
	h.serveBrowseRoots(index, httptest.NewRequest(http.MethodGet, "http://x/folder/", nil), "/folder/", roots, "", false)
	if !bytes.Equal(index.Body.Bytes(), indexBytes) || index.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("index: body=%q headers=%v", index.Body.Bytes(), index.Header())
	}
	indexRaw := httptest.NewRecorder()
	h.serveBrowseRoots(indexRaw, httptest.NewRequest(http.MethodGet, "http://x/folder/?raw", nil), "/folder/", roots, "", false)
	if !bytes.Equal(indexRaw.Body.Bytes(), indexBytes) || indexRaw.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("raw index: body=%q headers=%v", indexRaw.Body.Bytes(), indexRaw.Header())
	}

	for _, test := range []struct {
		cache string
		want  string
	}{
		{filesCacheNever, "no-store"},
		{filesCacheRevalidate, "no-cache"},
		{filesCacheForever, "public, max-age=31536000, immutable"},
	} {
		out := httptest.NewRecorder()
		h.serveBrowseRoots(out, httptest.NewRequest(http.MethodGet, "http://x/z.txt", nil), "/z.txt",
			[]activeBrowseRoot{{path: root, cache: test.cache}}, "", false)
		if got := out.Header().Get("Cache-Control"); got != test.want {
			t.Errorf("cache %s: got %q want %q", test.cache, got, test.want)
		}
	}
	for path, contentType := range map[string]string{
		"/image.png": "image/png", "/audio.mp3": "audio/mpeg", "/document.pdf": "application/pdf",
	} {
		out := httptest.NewRecorder()
		h.serveBrowseRoots(out, httptest.NewRequest(http.MethodGet, "http://x"+path, nil), path,
			[]activeBrowseRoot{{path: root, cache: filesCacheRevalidate, browse: true}}, "", false)
		if got := out.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
			t.Errorf("%s MIME: got %q want %q", path, got, contentType)
		}
	}
	rip, err := os.Open(filepath.Join(root, "folder", "index.rip"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := rip.Stat()
	if err != nil {
		t.Fatal(err)
	}
	mimeOut := httptest.NewRecorder()
	serveOpenedFile(mimeOut, httptest.NewRequest(http.MethodGet, "http://x/index.rip", nil),
		rip, info, "index.rip", filesCacheForever)
	rip.Close()
	if mimeOut.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
		mimeOut.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("MIME/cache coupling: %v", mimeOut.Header())
	}
}

func TestBrowseListingBounds(t *testing.T) {
	root := t.TempDir()
	for i := 0; i <= browseListingMaxEntries; i++ {
		name := fmt.Sprintf("%05d", i)
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newBrowseTestHandler(t, nil)
	roots := []activeBrowseRoot{{path: root, browse: true}}
	tooMany := httptest.NewRecorder()
	h.serveBrowseRoots(tooMany, httptest.NewRequest(http.MethodGet, "http://x/", nil), "/", roots, "", false)
	if tooMany.Code != http.StatusServiceUnavailable || tooMany.Header().Get("Cache-Control") != "no-store" ||
		tooMany.Body.String() != "Service Unavailable\n" {
		t.Fatalf("entry cap: status=%d headers=%v body=%q", tooMany.Code, tooMany.Header(), tooMany.Body.String())
	}
	if h.app.state.browse.counters.listings.Load() != 0 {
		t.Fatal("rejected entry count incremented listings")
	}

	oversized, err := template.New("index.html").Parse(strings.Repeat("x", browseListingMaxHTML+1))
	if err != nil {
		t.Fatal(err)
	}
	h.browseCfg.theme.template = oversized
	htmlTooLarge := httptest.NewRecorder()
	emptyRoot := t.TempDir()
	h.serveBrowseRoots(htmlTooLarge, httptest.NewRequest(http.MethodGet, "http://x/", nil), "/",
		[]activeBrowseRoot{{path: emptyRoot, browse: true}}, "", false)
	if htmlTooLarge.Code != http.StatusServiceUnavailable || htmlTooLarge.Header().Get("Cache-Control") != "no-store" ||
		htmlTooLarge.Body.String() != "Service Unavailable\n" {
		t.Fatalf("HTML cap: status=%d headers=%v body=%q", htmlTooLarge.Code, htmlTooLarge.Header(), htmlTooLarge.Body.String())
	}
	if h.app.state.browse.counters.listings.Load() != 0 {
		t.Fatal("oversized HTML incremented listings")
	}
}

func TestBrowseThemeAssets(t *testing.T) {
	h := newBrowseTestHandler(t, nil)
	target := browseAssetPrefix + h.browseCfg.theme.hash + "/browse.css"
	get := httptest.NewRecorder()
	h.serveBrowseAsset(get, httptest.NewRequest(http.MethodGet, target, nil))
	if get.Code != http.StatusOK || get.Body.Len() == 0 ||
		get.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		get.Header().Get("ETag") == "" {
		t.Fatalf("GET asset: %d %v len=%d", get.Code, get.Header(), get.Body.Len())
	}
	head := httptest.NewRecorder()
	h.serveBrowseAsset(head, httptest.NewRequest(http.MethodHead, target, nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 ||
		head.Header().Get("Content-Length") != get.Header().Get("Content-Length") {
		t.Fatalf("HEAD asset: %d %v len=%d", head.Code, head.Header(), head.Body.Len())
	}
	notModifiedRequest := httptest.NewRequest(http.MethodGet, target, nil)
	notModifiedRequest.Header.Set("If-None-Match", `W/`+get.Header().Get("ETag"))
	notModified := httptest.NewRecorder()
	h.serveBrowseAsset(notModified, notModifiedRequest)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 ||
		notModified.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		notModified.Header().Get("ETag") != get.Header().Get("ETag") ||
		notModified.Header().Get("Content-Length") != "" {
		t.Fatalf("conditional asset: %d %v len=%d", notModified.Code, notModified.Header(), notModified.Body.Len())
	}
	for _, target := range []string{
		browseAssetPrefix + "000000000000000000000000/browse.css",
		browseAssetPrefix + h.browseCfg.theme.hash + "/../index.html",
	} {
		out := httptest.NewRecorder()
		h.serveBrowseAsset(out, httptest.NewRequest(http.MethodGet, target, nil))
		if out.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", target, out.Code)
		}
	}
}

func TestBrowseThemeAssetsRequireResolvedHost(t *testing.T) {
	h := newBrowseTestHandler(t, nil)
	registry := newAppRegistry()
	if _, err := registry.create("known", []string{"known.test"}, ""); err != nil {
		t.Fatal(err)
	}
	h.dp = newDataPlane(registry, zap.NewNop())
	target := browseAssetPrefix + h.browseCfg.theme.hash + "/browse.css"

	unknown := httptest.NewRecorder()
	err := h.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "http://unknown.test"+target, nil), nil)
	var handlerError caddyhttp.HandlerError
	if !errors.As(err, &handlerError) || handlerError.StatusCode != http.StatusNotFound || unknown.Body.Len() != 0 {
		t.Fatalf("unknown host asset: err=%v response=%d %q", err, unknown.Code, unknown.Body.String())
	}

	known := httptest.NewRecorder()
	if err := h.ServeHTTP(known, httptest.NewRequest(http.MethodGet, "http://known.test"+target, nil), nil); err != nil ||
		known.Code != http.StatusOK {
		t.Fatalf("hot host asset: err=%v response=%d %q", err, known.Code, known.Body.String())
	}

	h.coldRoots = []BrowseRoot{{Path: t.TempDir()}}
	cold := httptest.NewRecorder()
	if err := h.ServeHTTP(cold, httptest.NewRequest(http.MethodGet, "http://cold.test"+target, nil), nil); err != nil ||
		cold.Code != http.StatusOK {
		t.Fatalf("cold host asset: err=%v response=%d %q", err, cold.Code, cold.Body.String())
	}
}

func TestBrowseColdRoutingUsesActiveHandler(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cold.txt"), []byte("cold-root"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newBrowseTestHandler(t, nil)
	h.coldRoots = []BrowseRoot{{Path: root}}
	h.dp = newDataPlane(newAppRegistry(), zap.NewNop())
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://cold.test/cold.txt", nil)
	if err := h.ServeHTTP(response, request, nil); err != nil || response.Code != http.StatusOK ||
		response.Body.String() != "cold-root" {
		t.Fatalf("cold handler routing: err=%v status=%d body=%q", err, response.Code, response.Body.String())
	}
	post := httptest.NewRecorder()
	if err := h.ServeHTTP(post, httptest.NewRequest(http.MethodPost, "http://cold.test/cold.txt", nil), nil); err != nil || post.Code != http.StatusNotFound {
		t.Fatalf("cold non-read method: err=%v status=%d body=%q", err, post.Code, post.Body.String())
	}
}

func TestBrowseRawSelectionForms(t *testing.T) {
	for query, want := range map[string]bool{
		"": false, "raw": true, "x=1&raw": true, "raw&%ZZ": true,
		"raw=": false, "raw=x": false, "%72aw": false, "x=1&raw=x": false,
	} {
		if got := rawBrowseBypass(query); got != want {
			t.Errorf("%q: got %v want %v", query, got, want)
		}
	}
	if got := browseRawURL("/a%20b", "x=%ZZ"); got != "/a%20b?x=%ZZ&raw" {
		t.Fatalf("raw URL: %q", got)
	}
	crumbs := browseBreadcrumbs("/a//b/", "/a//b/")
	if got := crumbs[len(crumbs)-1].URL; got != "/a//b/" {
		t.Fatalf("repeated-slash breadcrumb: %q", got)
	}
}

func TestBrowseRootCacheDefaultAndTerminalShellRules(t *testing.T) {
	policy, err := normalizeFilesPolicy(&FilesPolicy{
		Roots: []FilesRoot{{Path: "/tmp", Browse: true}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Roots[0].Cache != filesCacheRevalidate {
		t.Fatalf("cache default: %q", policy.Roots[0].Cache)
	}
	registry := newAppRegistry()
	if _, err := registry.createWithLease("browse", []string{"browse.test"}, nil, policy, "", nil, "heartbeat"); err != nil {
		t.Fatalf("terminal browse-only registration: %v", err)
	}
	if _, err := registry.createWithLease("worker", []string{"worker.test"}, nil, policy, "",
		[]Upstream{{Path: "/tmp/worker.sock"}}, "heartbeat"); err == nil {
		t.Fatal("terminal browse-only registration accepted upstreams")
	}
	if _, err := normalizeFilesPolicy(&FilesPolicy{
		Roots: []FilesRoot{{Path: "/tmp"}},
	}, false); err == nil {
		t.Fatal("non-browsable root omitted shell")
	}
	for _, test := range []struct {
		source string
		want   string
	}{
		{`{"path":"/tmp","browse":null}`, "files root browse must be a boolean"},
		{`{"path":"/tmp","cache":null,"browse":true}`, "files root cache must be a string"},
		{`{"path":"/tmp","browse":true,"class":"live"}`, `unknown field "class"`},
	} {
		var root FilesRoot
		err := json.Unmarshal([]byte(test.source), &root)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: got %v, want error containing %q", test.source, err, test.want)
		}
	}

	mux := newTestControlMux(t)
	for _, body := range []string{
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","browse":null}]}}`,
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","cache":null,"browse":true}]}}`,
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","browse":"yes"}]}}`,
		`{"name":"x","hosts":["x.test"],"files":{"roots":[{"path":"/tmp","cache":"sometimes","browse":true}]}}`,
	} {
		if code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps", body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d", body, code)
		}
	}
	code, created := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"browse","hosts":["browse.test"],"files":{"roots":[{"path":"/tmp","browse":true}]}}`)
	if code != http.StatusCreated {
		t.Fatalf("terminal create: %d %v", code, created)
	}
	code, record := doJSON(t, mux, http.MethodGet, "/1.0/apps/"+created["id"].(string), "")
	if code != http.StatusOK {
		t.Fatalf("get normalized app: %d %v", code, record)
	}
	files := record["files"].(map[string]any)
	root := files["roots"].([]any)[0].(map[string]any)
	if root["browse"] != true || root["cache"] != "revalidate" || record["lease"] != "heartbeat" {
		t.Fatalf("normalized app JSON: %d %v", code, record)
	}
}

func TestBrowseRendererExecutionEnvironmentAndFailures(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	file := filepath.Join(root, "note.md")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	renderer := &browseRenderer{
		extension: ".md", executable: executable,
		args:        []string{"-test.run=TestBrowseRendererHelper", "--", "{file}"},
		contentType: "text/plain; charset=utf-8", timeout: 2 * time.Second,
		maxOutput: 1 << 20, concurrency: 1, id: "helper",
	}
	h := newBrowseTestHandler(t, map[string]*browseRenderer{".md": renderer})
	t.Setenv("GO_WANT_BROWSE_HELPER", "env")
	head := httptest.NewRecorder()
	h.serveBrowseRoots(head, httptest.NewRequest(http.MethodHead, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, cache: filesCacheRevalidate, browse: true}}, "", false)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "" {
		t.Fatalf("renderer HEAD: %d %v len=%d", head.Code, head.Header(), head.Body.Len())
	}
	if h.app.state.browse.counters.renderStarts.Load() != 0 {
		t.Fatal("renderer HEAD spawned")
	}
	release, ok := h.app.state.browse.admit(renderer, browseDefaultConcurrency)
	if !ok {
		t.Fatal("fixture admission rejected")
	}
	saturated := httptest.NewRecorder()
	h.serveBrowseRoots(saturated, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	release()
	if saturated.Code != http.StatusServiceUnavailable ||
		saturated.Header().Get("Retry-After") != "1" ||
		saturated.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("saturation: %d %v", saturated.Code, saturated.Header())
	}
	out := httptest.NewRecorder()
	h.serveBrowseRoots(out, httptest.NewRequest(http.MethodGet, "http://x/note.md?q=1", nil), "/note.md",
		[]activeBrowseRoot{{path: root, cache: filesCacheRevalidate, browse: true}}, "", false)
	if out.Code != http.StatusOK {
		t.Fatalf("render: %d %s", out.Code, out.Body.String())
	}
	for _, want := range []string{file, "/note.md", "/note.md?q=1&raw", root, "hello", "env"} {
		if !strings.Contains(out.Body.String(), want) {
			t.Errorf("renderer output missing %q: %s", want, out.Body.String())
		}
	}

	t.Setenv("BROWSE_HELPER_MODE", "pipes")
	pipes := httptest.NewRecorder()
	h.serveBrowseRoots(pipes, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if pipes.Code != http.StatusOK || pipes.Body.Len() != 256<<10 {
		t.Fatalf("renderer pipe drain: %d bytes=%d body=%q", pipes.Code, pipes.Body.Len(), pipes.Body.String())
	}
	t.Setenv("BROWSE_HELPER_MODE", "")

	raw := httptest.NewRecorder()
	h.serveBrowseRoots(raw, httptest.NewRequest(http.MethodGet, "http://x/note.md?raw", nil), "/note.md",
		[]activeBrowseRoot{{path: root, cache: filesCacheNever, browse: true}}, "", false)
	if raw.Body.String() != "hello" || raw.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("raw: body=%q headers=%v", raw.Body.String(), raw.Header())
	}

	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "index.rip"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	ripRenderer := *renderer
	ripRenderer.extension = ".rip"
	ripRenderer.id = "helper-rip"
	h.app.Browse.renderers[".rip"] = &ripRenderer
	index := httptest.NewRecorder()
	h.serveBrowseRoots(index, httptest.NewRequest(http.MethodGet, "http://x/sub/?q=1", nil), "/sub/",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	for _, want := range []string{filepath.Join(root, "sub", "index.rip"), "/sub/", "/sub/?q=1&raw"} {
		if !strings.Contains(index.Body.String(), want) {
			t.Errorf("index renderer output missing %q: %s", want, index.Body.String())
		}
	}
	indexRaw := httptest.NewRecorder()
	h.serveBrowseRoots(indexRaw, httptest.NewRequest(http.MethodGet, "http://x/sub/?raw", nil), "/sub/",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if indexRaw.Body.String() != "index" {
		t.Fatalf("index raw body=%q", indexRaw.Body.String())
	}

	renderer.maxOutput = 2
	overflow := httptest.NewRecorder()
	h.serveBrowseRoots(overflow, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if overflow.Code != http.StatusBadGateway {
		t.Fatalf("overflow: %d %s", overflow.Code, overflow.Body.String())
	}

	renderer.maxOutput = 1 << 20
	t.Setenv("BROWSE_HELPER_MODE", "fail")
	failed := httptest.NewRecorder()
	h.serveBrowseRoots(failed, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if failed.Code != http.StatusBadGateway || strings.Contains(failed.Body.String(), "secret") {
		t.Fatalf("failure: %d %s", failed.Code, failed.Body.String())
	}

	t.Setenv("BROWSE_HELPER_MODE", "")
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRequest := httptest.NewRequest(http.MethodGet, "http://x/note.md", nil).WithContext(canceledContext)
	canceled := httptest.NewRecorder()
	h.serveBrowseRoots(canceled, canceledRequest, "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if canceled.Body.Len() != 0 || len(canceled.Header()) != 0 {
		t.Fatalf("client cancellation wrote replacement response: %v %q", canceled.Header(), canceled.Body.String())
	}

	if runtime.GOOS != "windows" {
		renderer.timeout = 300 * time.Millisecond
		pidFile := filepath.Join(t.TempDir(), "descendant.pid")
		t.Setenv("BROWSE_DESCENDANT_PID_FILE", pidFile)
		t.Setenv("BROWSE_HELPER_MODE", "descendant")
		descendant := httptest.NewRecorder()
		h.serveBrowseRoots(descendant, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
			[]activeBrowseRoot{{path: root, browse: true}}, "", false)
		if descendant.Code != http.StatusGatewayTimeout {
			t.Fatalf("descendant timeout: %d %s", descendant.Code, descendant.Body.String())
		}
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(string(pidBytes))
		if err != nil {
			t.Fatal(err)
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for process.Signal(syscall.Signal(0)) == nil && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if process.Signal(syscall.Signal(0)) == nil {
			t.Fatalf("renderer descendant %d survived cancellation", pid)
		}
	}

	renderer.timeout = 20 * time.Millisecond
	t.Setenv("BROWSE_HELPER_MODE", "sleep")
	timeout := httptest.NewRecorder()
	h.serveBrowseRoots(timeout, httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md",
		[]activeBrowseRoot{{path: root, browse: true}}, "", false)
	if timeout.Code != http.StatusGatewayTimeout || timeout.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("timeout: %d %v %s", timeout.Code, timeout.Header(), timeout.Body.String())
	}
}

func TestBrowseRendererHelper(t *testing.T) {
	if os.Getenv("GO_WANT_BROWSE_HELPER") == "" {
		return
	}
	if os.Getenv("BROWSE_HELPER_MODE") == "sleep" {
		time.Sleep(time.Second)
	}
	if os.Getenv("BROWSE_HELPER_MODE") == "fail" {
		_, _ = fmt.Fprint(os.Stderr, "secret stderr")
		os.Exit(7)
	}
	if os.Getenv("BROWSE_HELPER_MODE") == "pipes" {
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), 256<<10))
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("e"), 256<<10))
		os.Exit(0)
	}
	if os.Getenv("BROWSE_HELPER_MODE") == "descendant" {
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("BROWSE_DESCENDANT_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(4)
		}
		time.Sleep(30 * time.Second)
	}
	data, err := os.ReadFile(os.Getenv("JANUS_BROWSE_FILE"))
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("%s\n%s\n%s\n%s\n%s\n%s", os.Getenv("JANUS_BROWSE_FILE"),
		os.Getenv("JANUS_BROWSE_URL"), os.Getenv("JANUS_BROWSE_RAW_URL"),
		os.Getenv("JANUS_BROWSE_ROOT"), data, os.Getenv("GO_WANT_BROWSE_HELPER"))
	os.Exit(0)
}

func TestBrowseSupervisorAdmissionAcrossDefinitions(t *testing.T) {
	supervisor := newBrowseSupervisor()
	first := &browseRenderer{extension: ".md", concurrency: 1, id: "old"}
	second := &browseRenderer{extension: ".md", concurrency: 2, id: "new"}
	releaseFirst, ok := supervisor.admit(first, 2)
	if !ok {
		t.Fatal("first admission rejected")
	}
	releaseSecond, ok := supervisor.admit(second, 2)
	if !ok {
		t.Fatal("second generation did not count old child under its extension limit")
	}
	if _, ok := supervisor.admit(second, 3); ok {
		t.Fatal("extension saturation queued or admitted")
	}
	releaseSecond()
	releaseFirst()
	if supervisor.running != 0 || len(supervisor.byRenderer) != 0 {
		t.Fatalf("slots leaked: %+v", supervisor)
	}
}

func TestBrowseCountersExact(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	renderer := &browseRenderer{
		extension: ".md", executable: executable,
		args:        []string{"-test.run=TestBrowseRendererHelper", "--", "{file}"},
		contentType: "text/plain", timeout: 5 * time.Second,
		maxOutput: 1 << 20, concurrency: 1, id: "counter",
	}
	h := newBrowseTestHandler(t, map[string]*browseRenderer{".md": renderer})
	t.Setenv("GO_WANT_BROWSE_HELPER", "counter")
	roots := []activeBrowseRoot{{path: root, browse: true}}

	h.serveBrowseRoots(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/", nil), "/", roots, "", false)
	h.serveBrowseRoots(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "http://x/note.md", nil), "/note.md", roots, "", false)
	h.serveBrowseRoots(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/note.md?raw", nil), "/note.md", roots, "", false)
	h.serveBrowseRoots(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://x/note.md", nil), "/note.md", roots, "", false)

	counters := &h.app.state.browse.counters
	if counters.listings.Load() != 1 || counters.rawBypasses.Load() != 1 ||
		counters.renderAttempts.Load() != 1 || counters.renderStarts.Load() != 1 ||
		counters.renderSuccesses.Load() != 1 || counters.renderFailures.Load() != 0 ||
		counters.renderTimeouts.Load() != 0 || counters.renderOverflows.Load() != 0 ||
		counters.renderCancellations.Load() != 0 || counters.renderSaturations.Load() != 0 {
		t.Fatalf("unexpected counters: %+v", h.app.browseStatus()["counters"])
	}
}

func TestBrowseLeaseJSONLifecycle(t *testing.T) {
	mux := newTestControlMux(t)
	for _, body := range []string{
		`{"name":"x","hosts":["x.test"],"lease":null}`,
		`{"name":"x","hosts":["x.test"],"lease":""}`,
		`{"name":"x","hosts":["x.test"],"lease":"forever"}`,
		`{"name":"x","hosts":["x.test"],"lease":1}`,
	} {
		if code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps", body); code != http.StatusBadRequest {
			t.Errorf("%s: got %d", body, code)
		}
	}
	code, created := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"proc","hosts":["proc.test"],"lease":"process"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)
	if code, _ := doJSON(t, mux, http.MethodPost, "/1.0/apps/"+id+"/heartbeat", ""); code != http.StatusBadRequest {
		t.Fatalf("process heartbeat: %d", code)
	}
	code, got := doJSON(t, mux, http.MethodGet, "/1.0/apps/"+id, "")
	if code != http.StatusOK || got["lease"] != "process" {
		t.Fatalf("get: %d %v", code, got)
	}
}

func TestBrowseProcessLeaseSkipsSweep(t *testing.T) {
	registry := newAppRegistry()
	now := time.Unix(100, 0)
	registry.now = func() time.Time { return now }
	registry.ttl = time.Second
	heartbeat, err := registry.create("heart", []string{"heart.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	process, err := registry.createWithLease("proc", []string{"proc.test"}, nil, nil, "", nil, "process")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if got := registry.sweepExpired(); !reflect.DeepEqual(got, []string{heartbeat.ID}) {
		t.Fatalf("reaped %v", got)
	}
	if _, err := registry.get(process.ID); err != nil {
		t.Fatal("process lease reaped")
	}
	stored, err := registry.get(process.ID)
	if err != nil || !stored.heartbeatAt.IsZero() {
		t.Fatalf("process lease has heartbeat clock: %+v err=%v", stored.heartbeatAt, err)
	}
	if err := registry.heartbeat(process.ID); err == nil {
		t.Fatal("process heartbeat accepted")
	}
}

func TestBrowseColdClaimsConflictsAndTLSAsk(t *testing.T) {
	supervisor := newBrowseSupervisor()
	registry := newAppRegistry()
	registry.browse = supervisor
	old := new(App)
	if err := supervisor.reserveCold(old, []string{"cold.test"}, registry); err != nil {
		t.Fatal(err)
	}
	next := new(App)
	if err := supervisor.reserveCold(next, []string{"cold.test"}, registry); err != nil {
		t.Fatalf("overlapping generation rejected: %v", err)
	}
	if _, err := registry.create("hot", []string{"cold.test"}, ""); err == nil {
		t.Fatal("hot registration claimed cold host")
	}
	supervisor.releaseCold(next)
	supervisor.releaseCold(old)
	if _, err := registry.create("hot", []string{"cold.test"}, ""); err != nil {
		t.Fatalf("released cold claim remained: %v", err)
	}

	registry = newAppRegistry()
	registry.browse = supervisor
	app := &App{appsReg: registry, state: &janusState{browse: supervisor}}
	if err := supervisor.reserveCold(app, []string{"ask.test"}, registry); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/1.0/tls/ask?domain=ASK.TEST", nil)
	response := httptest.NewRecorder()
	app.handleTLSAsk(response, request)
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body["claim"] != "cold" || body["domain"] != "ask.test" {
		t.Fatalf("TLS ask: %d %v", response.Code, body)
	}
}

func TestBrowseExactHostIntrospection(t *testing.T) {
	hosts, err := exactBrowseHosts([][]string{{"B.TEST", "a.test"}, {"a.test", "b.test"}})
	if err != nil || !reflect.DeepEqual(hosts, []string{"a.test", "b.test"}) {
		t.Fatalf("exact hosts: %v %v", hosts, err)
	}
	for _, alternatives := range [][][]string{
		nil,
		{{"*.test"}},
		{{"127.0.0.1"}},
		{{"a.test"}, {"b.test"}},
	} {
		if _, err := exactBrowseHosts(alternatives); err == nil {
			t.Errorf("accepted alternatives %v", alternatives)
		}
	}
}

func TestBrowseStatusRedactsCommandsAndPaths(t *testing.T) {
	h := newBrowseTestHandler(t, nil)
	renderer := &browseRenderer{
		extension: ".md", executable: "/secret/bin", args: []string{"--secret"},
		contentType: "text/html", timeout: time.Second, maxOutput: 12,
		concurrency: 2, id: "active",
	}
	h.app.Browse.renderers = map[string]*browseRenderer{".md": renderer}
	h.app.browseSites = []browseSiteEntry{{
		hosts: []string{"cold.test"}, enabled: true,
		handler: &Handler{coldRoots: []BrowseRoot{{Path: "/secret/root", Cache: filesCacheRevalidate}}},
	}}
	status, err := json.Marshal(h.app.browseStatus())
	if err != nil {
		t.Fatal(err)
	}
	text := string(status)
	for _, secret := range []string{"/secret/bin", "--secret", "/secret/root"} {
		if strings.Contains(text, secret) {
			t.Fatalf("status leaked %q: %s", secret, text)
		}
	}
	for _, want := range []string{`"enabled":true`, `"extension":".md"`, `"host":"cold.test"`} {
		if !strings.Contains(text, want) {
			t.Errorf("status missing %q: %s", want, text)
		}
	}
}

func TestBrowseSupervisorReleaseIsIdempotent(t *testing.T) {
	supervisor := newBrowseSupervisor()
	renderer := &browseRenderer{extension: ".x", concurrency: 1, id: "x"}
	release, ok := supervisor.admit(renderer, 1)
	if !ok {
		t.Fatal("admission rejected")
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			release()
		}()
	}
	group.Wait()
	if supervisor.running != 0 {
		t.Fatalf("running=%d", supervisor.running)
	}
}

func BenchmarkBrowseRegularFile(b *testing.B) {
	root := b.TempDir()
	payload := bytes.Repeat([]byte("x"), 64<<10)
	if err := os.WriteFile(filepath.Join(root, "file.bin"), payload, 0o644); err != nil {
		b.Fatal(err)
	}
	for _, benchmark := range []struct {
		name          string
		browseEnabled bool
		rootBrowse    bool
	}{
		{"BrowseOff", false, true},
		{"BrowseFalse", true, false},
		{"BrowseTrue", true, true},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			h := newBrowseTestHandler(b, nil)
			h.browseEnabled = benchmark.browseEnabled
			roots := []activeBrowseRoot{{path: root, cache: filesCacheRevalidate, browse: benchmark.rootBrowse}}
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for b.Loop() {
				out := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "http://bench.test/file.bin", nil)
				if !h.serveBrowseRoots(out, request, "/file.bin", roots, "", false) || out.Body.Len() != len(payload) {
					b.Fatal("file delivery failed")
				}
			}
		})
	}
}

func BenchmarkBrowseListing(b *testing.B) {
	for _, count := range []int{10, 100, 1000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			root := b.TempDir()
			for i := 0; i < count; i++ {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%04d.txt", i)), []byte("x"), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			h := newBrowseTestHandler(b, nil)
			roots := []activeBrowseRoot{{path: root, browse: true}}
			b.ReportAllocs()
			for b.Loop() {
				out := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "http://bench.test/", nil)
				if !h.serveBrowseRoots(out, request, "/", roots, "", false) || out.Code != http.StatusOK {
					b.Fatal("listing failed")
				}
			}
		})
	}
}

func BenchmarkBrowseThemeAsset(b *testing.B) {
	h := newBrowseTestHandler(b, nil)
	target := browseAssetPrefix + h.browseCfg.theme.hash + "/browse.css"
	b.ReportAllocs()
	b.SetBytes(int64(len(h.browseCfg.theme.assets["browse.css"].data)))
	for b.Loop() {
		out := httptest.NewRecorder()
		h.serveBrowseAsset(out, httptest.NewRequest(http.MethodGet, target, nil))
		if out.Code != http.StatusOK {
			b.Fatal("theme asset failed")
		}
	}
}

func BenchmarkBrowseRenderer(b *testing.B) {
	root := b.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.bench"), nil, 0o644); err != nil {
		b.Fatal(err)
	}
	run := func(b *testing.B, executable string, args []string) {
		renderer := &browseRenderer{
			extension: ".bench", executable: executable, args: args,
			contentType: "text/plain", timeout: 10 * time.Second,
			maxOutput: 1 << 20, concurrency: 1, id: executable,
		}
		h := newBrowseTestHandler(b, map[string]*browseRenderer{".bench": renderer})
		roots := []activeBrowseRoot{{path: root, browse: true}}
		b.ReportAllocs()
		for b.Loop() {
			out := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://bench.test/file.bench", nil)
			if !h.serveBrowseRoots(out, request, "/file.bench", roots, "", false) || out.Code != http.StatusOK {
				b.Fatal("renderer failed")
			}
		}
	}
	b.Run("NoopChild", func(b *testing.B) {
		executable, err := exec.LookPath("true")
		if err != nil {
			b.Skip(err)
		}
		run(b, executable, nil)
	})
	b.Run("Bun", func(b *testing.B) {
		executable, err := exec.LookPath("bun")
		if err != nil {
			b.Skip("bun is not installed")
		}
		run(b, executable, []string{"-e", `process.stdout.write("")`, "{file}"})
	})
}

func BenchmarkBrowseThemeProvisioning(b *testing.B) {
	embedded, err := loadBrowseTheme("")
	if err != nil {
		b.Fatal(err)
	}
	custom := b.TempDir()
	for name, asset := range embedded.assets {
		if err := os.WriteFile(filepath.Join(custom, name), asset.data, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	for _, benchmark := range []struct {
		name  string
		theme string
	}{
		{"Embedded", ""},
		{"Custom", custom},
	} {
		b.Run(benchmark.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				app := &App{Browse: &BrowseSettings{Theme: benchmark.theme}}
				if err := app.provisionBrowse(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
