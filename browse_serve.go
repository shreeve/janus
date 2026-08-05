package janus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

var browseIndexes = []string{"index.html", "index.rip", "index.ts", "index.tsx", "index.jsx", "index.js"}

const (
	browseListingMaxEntries = 10_000
	browseListingMaxHTML    = 16 << 20
	browseListingChunkSize  = 32 << 10
)

var errBrowseListingTooLarge = errors.New("directory listing too large")

func (h *Handler) serveBrowseAsset(w http.ResponseWriter, r *http.Request) {
	accessFactsOf(r).setClass("browse_asset")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, browseAssetPrefix)
	hash, name, ok := strings.Cut(rest, "/")
	if !ok || len(hash) != 24 || name == "" || strings.Contains(name, `\`) {
		http.NotFound(w, r)
		return
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == "" || segment == "." || segment == ".." {
			http.NotFound(w, r)
			return
		}
	}
	theme := h.browseCfg.theme
	current := theme != nil && theme.hash == hash
	if !current {
		theme = h.app.state.browse.theme(hash)
	}
	if theme == nil {
		http.NotFound(w, r)
		return
	}
	asset, ok := theme.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if current {
		h.app.state.browse.retainTheme(theme)
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", asset.etag)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if browseIfNoneMatch(r.Header.Get("If-None-Match"), asset.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(asset.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(asset.data)
	}
}

func browseIfNoneMatch(value, etag string) bool {
	for _, candidate := range splitHeaderList(value) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func (h *Handler) serveColdBrowse(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return nil
	}
	requestPath, err := validatedRequestPath(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil
	}
	if h.serveBrowseRoots(w, r, requestPath, coldBrowseRoots(h.coldRoots), "", false) {
		return nil
	}
	http.NotFound(w, r)
	return nil
}

type activeBrowseRoot struct {
	path   string
	cache  string
	browse bool
}

func coldBrowseRoots(roots []BrowseRoot) []activeBrowseRoot {
	out := make([]activeBrowseRoot, 0, len(roots))
	for _, root := range roots {
		cache := root.Cache
		if cache == "" {
			cache = filesCacheRevalidate
		}
		out = append(out, activeBrowseRoot{path: root.Path, cache: cache, browse: true})
	}
	return out
}

func hotBrowseRoots(roots []FilesRoot, site string) []activeBrowseRoot {
	out := make([]activeBrowseRoot, 0, len(roots))
	for _, root := range roots {
		cache := root.Cache
		if cache == "" {
			cache = filesCacheRevalidate
		}
		out = append(out, activeBrowseRoot{
			path:  strings.ReplaceAll(root.Path, "{site}", site),
			cache: cache, browse: root.Browse,
		})
	}
	return out
}

func (h *Handler) serveBrowseRoots(w http.ResponseWriter, r *http.Request, requestPath string, roots []activeBrowseRoot, shell string, allowShell bool) bool {
	return h.serveBrowseRootsPrecompressed(w, r, requestPath, roots, shell, allowShell, nil)
}

func (h *Handler) serveBrowseRootsPrecompressed(w http.ResponseWriter, r *http.Request, requestPath string, roots []activeBrowseRoot, shell string, allowShell bool, precompressed []string) bool {
	relative := strings.TrimPrefix(requestPath, "/")
	if relative == "" {
		relative = "."
	}
	for _, configured := range roots {
		root, err := os.OpenRoot(configured.path)
		if err != nil {
			continue
		}
		file, err := root.Open(relative)
		if err != nil {
			root.Close()
			continue
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			root.Close()
			continue
		}
		if info.Mode().IsRegular() {
			if configured.browse && h.browseEnabled {
				if h.serveBrowseFile(w, r, file, info, configured, relative, requestPath) {
					root.Close()
					return true
				}
			}
			serveOpenedFileFromRoot(w, r, root, file, info, relative, requestPath, configured.cache, precompressed)
			file.Close()
			root.Close()
			return true
		}
		file.Close()
		if !info.IsDir() || !configured.browse || !h.browseEnabled {
			root.Close()
			continue
		}
		if !strings.HasSuffix(requestPath, "/") {
			location := r.URL.EscapedPath() + "/"
			if r.URL.RawQuery != "" {
				location += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, location, http.StatusPermanentRedirect)
			root.Close()
			return true
		}
		for _, index := range browseIndexes {
			indexRelative := index
			if relative != "." {
				indexRelative = relative + "/" + index
			}
			indexFile, err := root.Open(indexRelative)
			if err != nil {
				continue
			}
			indexInfo, err := indexFile.Stat()
			if err != nil || !indexInfo.Mode().IsRegular() {
				indexFile.Close()
				continue
			}
			if h.serveBrowseFile(w, r, indexFile, indexInfo, configured, indexRelative, requestPath) {
				root.Close()
				return true
			}
			serveOpenedFileFromRoot(w, r, root, indexFile, indexInfo, indexRelative, indexRelative, configured.cache, precompressed)
			indexFile.Close()
			root.Close()
			return true
		}
		h.serveBrowseListing(w, r, root, configured, relative, requestPath)
		root.Close()
		return true
	}
	if allowShell && shell != "" && acceptsHTML(r.Header.Get("Accept")) {
		return serveAbsoluteFile(w, r, shell, precompressed)
	}
	return false
}

func rawBrowseBypass(rawQuery string) bool {
	for _, component := range strings.Split(rawQuery, "&") {
		if component == "raw" {
			return true
		}
	}
	return false
}

func browseRawURL(escapedPath, rawQuery string) string {
	if rawQuery == "" {
		return escapedPath + "?raw"
	}
	return escapedPath + "?" + rawQuery + "&raw"
}

func (h *Handler) serveBrowseFile(w http.ResponseWriter, r *http.Request, file *os.File, info os.FileInfo, root activeBrowseRoot, relative, requestPath string) bool {
	renderer := h.browseCfg.matchRenderer(relative)
	if renderer == nil {
		return false
	}
	if rawBrowseBypass(r.URL.RawQuery) {
		h.app.state.browse.counters.rawBypasses.Add(1)
		return false
	}
	if r.Method == http.MethodHead {
		file.Close()
		w.Header().Set("Content-Type", renderer.contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		return true
	}
	file.Close()
	h.serveRenderedFile(w, r, renderer, root.path, relative, requestPath)
	return true
}

func (h *Handler) serveBrowseListing(w http.ResponseWriter, r *http.Request, root *os.Root, configured activeBrowseRoot, relative, requestPath string) {
	accessFactsOf(r).setClass("browse_listing")
	directory, err := root.Open(relative)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer directory.Close()
	names, err := directory.Readdirnames(browseListingMaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "directory listing failed", http.StatusInternalServerError)
		return
	}
	if len(names) > browseListingMaxEntries {
		browseListingUnavailable(w)
		return
	}
	var entries []BrowseEntry
	baseEscaped := r.URL.EscapedPath()
	if !strings.HasSuffix(baseEscaped, "/") {
		baseEscaped += "/"
	}
	for _, name := range names {
		entryRelative := name
		if relative != "." {
			entryRelative = relative + "/" + name
		}
		info, err := root.Lstat(entryRelative)
		if err != nil {
			continue
		}
		entry := BrowseEntry{
			Name: name, Size: info.Size(), SizeText: humanize.Bytes(uint64(max(info.Size(), 0))),
			Modified: info.ModTime(), ModifiedText: info.ModTime().UTC().Format(time.RFC3339),
		}
		escaped := baseEscaped + url.PathEscape(name)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			entry.Kind, entry.Icon = "symlink", "symlink"
		case info.IsDir():
			entry.Kind, entry.Icon, entry.URL = "directory", "directory", escaped+"/"
		case info.Mode().IsRegular():
			entry.Kind, entry.Icon, entry.URL = "file", browseFileIcon(name), escaped
			if renderer := h.browseCfg.matchRenderer(name); renderer != nil {
				entry.Rendered = true
				entry.RawURL = browseRawURL(escaped, r.URL.RawQuery)
			}
			if browsePreviewable(name) {
				entry.PreviewURL = escaped
			}
		default:
			entry.Kind, entry.Icon = "other", "binary"
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		leftDir := entries[i].Kind == "directory"
		rightDir := entries[j].Kind == "directory"
		if leftDir != rightDir {
			return leftDir
		}
		return entries[i].Name < entries[j].Name
	})
	page := BrowsePage{
		Version: 1, Title: "Index of " + requestPath, Path: requestPath,
		RootName:    filepath.Base(configured.path),
		AssetBase:   browseAssetPrefix + h.browseCfg.theme.hash,
		Breadcrumbs: browseBreadcrumbs(requestPath, r.URL.EscapedPath()), Entries: entries,
	}
	if requestPath != "/" {
		parentPath := strings.TrimSuffix(r.URL.EscapedPath(), "/")
		parentPath = parentPath[:strings.LastIndex(parentPath, "/")+1]
		page.Parent = &BrowseLink{Name: "..", URL: parentPath}
	}
	body := newBrowseListingBuffer()
	if err := h.browseCfg.theme.template.ExecuteTemplate(&body, "index.html", page); err != nil {
		if errors.Is(err, errBrowseListingTooLarge) {
			browseListingUnavailable(w)
			return
		}
		http.Error(w, "directory listing failed", http.StatusInternalServerError)
		return
	}
	h.app.state.browse.retainTheme(h.browseCfg.theme)
	h.app.state.browse.counters.listings.Add(1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", fmt.Sprint(body.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = body.WriteTo(w)
	}
}

type browseListingBuffer struct {
	chunks [][]byte
	size   int
}

func newBrowseListingBuffer() browseListingBuffer {
	return browseListingBuffer{}
}

func (b *browseListingBuffer) Write(value []byte) (int, error) {
	written := 0
	for len(value) > 0 {
		if b.size == browseListingMaxHTML {
			return written, errBrowseListingTooLarge
		}
		if len(b.chunks) == 0 || len(b.chunks[len(b.chunks)-1]) == cap(b.chunks[len(b.chunks)-1]) {
			size := min(browseListingChunkSize, browseListingMaxHTML-b.size)
			b.chunks = append(b.chunks, make([]byte, 0, size))
		}
		last := len(b.chunks) - 1
		available := cap(b.chunks[last]) - len(b.chunks[last])
		count := min(len(value), available)
		b.chunks[last] = append(b.chunks[last], value[:count]...)
		b.size += count
		written += count
		value = value[count:]
	}
	return written, nil
}

func (b *browseListingBuffer) Len() int {
	return b.size
}

func (b *browseListingBuffer) WriteTo(w io.Writer) (int64, error) {
	var total int64
	for _, chunk := range b.chunks {
		written, err := w.Write(chunk)
		total += int64(written)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func browseListingUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
}

func browseBreadcrumbs(requestPath, escapedPath string) []BrowseLink {
	links := []BrowseLink{{Name: "/", URL: "/"}}
	decodedSegments := strings.Split(strings.TrimPrefix(requestPath, "/"), "/")
	escapedSegments := strings.Split(strings.TrimPrefix(escapedPath, "/"), "/")
	current := ""
	for i, segment := range decodedSegments {
		escaped := ""
		if i < len(escapedSegments) {
			escaped = escapedSegments[i]
		}
		current += "/" + escaped
		if segment == "" {
			continue
		}
		links = append(links, BrowseLink{Name: segment, URL: current + "/"})
	}
	return links
}

func browsePreviewable(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".avif":
		return true
	default:
		return false
	}
}

func browseFileIcon(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".svg", ".webp", ".avif":
		return "image"
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac":
		return "audio"
	case ".mp4", ".webm", ".mov", ".mkv", ".avi":
		return "video"
	case ".txt", ".md", ".html", ".htm", ".css", ".js", ".ts", ".tsx", ".jsx", ".json", ".rip", ".xml", ".yaml", ".yml":
		return "text"
	case ".zip", ".gz", ".bz2", ".xz", ".tar", ".tgz", ".7z", ".rar":
		return "archive"
	default:
		return "binary"
	}
}

type cappedBuffer struct {
	buffer   bytes.Buffer
	limit    int64
	overflow atomic.Bool
}

type cappedLogBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func (b *cappedLogBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		if int64(len(value)) > remaining {
			_, _ = b.buffer.Write(value[:remaining])
		} else {
			_, _ = b.buffer.Write(value)
		}
	}
	return len(value), nil
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - int64(b.buffer.Len())
	if remaining < int64(len(value)) {
		if remaining > 0 {
			_, _ = b.buffer.Write(value[:remaining])
		}
		b.overflow.Store(true)
		return len(value), errors.New("output limit exceeded")
	}
	_, _ = b.buffer.Write(value)
	return len(value), nil
}

func (h *Handler) serveRenderedFile(w http.ResponseWriter, r *http.Request, renderer *browseRenderer, rootPath, relative, requestPath string) {
	accessFactsOf(r).setClass("browse_render")
	totalLimit := browseDefaultConcurrency
	if h.browseCfg.Concurrency != nil {
		totalLimit = *h.browseCfg.Concurrency
	}
	release, ok := h.app.state.browse.admit(renderer, totalLimit)
	if !ok {
		w.Header().Set("Retry-After", "1")
		browseRenderError(w, http.StatusServiceUnavailable)
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), renderer.timeout)
	defer cancel()
	generationCanceled := atomic.Bool{}
	stopGeneration := context.AfterFunc(h.app.browseCtx, func() {
		generationCanceled.Store(true)
		cancel()
	})
	defer stopGeneration()

	selected := filepath.Join(rootPath, filepath.FromSlash(relative))
	args := make([]string, len(renderer.args))
	for i, arg := range renderer.args {
		if arg == "{file}" {
			args[i] = selected
		} else {
			args[i] = arg
		}
	}
	cmd := exec.CommandContext(ctx, renderer.executable, args...)
	configureBrowseProcess(cmd)
	cmd.Dir = filepath.Dir(selected)
	rawURL := browseRawURL(r.URL.EscapedPath(), r.URL.RawQuery)
	cmd.Env = browseRendererEnvironment(os.Environ(), map[string]string{
		"JANUS_BROWSE_FILE":    selected,
		"JANUS_BROWSE_URL":     r.URL.EscapedPath(),
		"JANUS_BROWSE_RAW_URL": rawURL,
		"JANUS_BROWSE_ROOT":    rootPath,
	})
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.renderStartFailure(w, renderer, err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		h.renderStartFailure(w, renderer, err)
		return
	}
	if ctx.Err() != nil {
		h.renderContextFailure(w, r, ctx, &generationCanceled)
		return
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			h.renderContextFailure(w, r, ctx, &generationCanceled)
			return
		}
		h.renderStartFailure(w, renderer, err)
		return
	}
	h.app.state.browse.counters.renderStarts.Add(1)

	var output cappedBuffer
	output.limit = renderer.maxOutput
	var errorOutput cappedLogBuffer
	errorOutput.limit = 64 << 10
	copyDone := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(&output, stdout)
		if output.overflow.Load() {
			cancel()
		}
		copyDone <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(&errorOutput, stderr)
		copyDone <- struct{}{}
	}()
	<-copyDone
	<-copyDone
	waitErr := cmd.Wait()

	counters := &h.app.state.browse.counters
	if output.overflow.Load() || ctx.Err() != nil || waitErr != nil {
		fields := []zap.Field{
			zap.String("extension", renderer.extension),
			zap.ByteString("stderr", errorOutput.buffer.Bytes()),
		}
		if cmd.ProcessState != nil {
			fields = append(fields, zap.Int("exit_status", cmd.ProcessState.ExitCode()))
		}
		if waitErr != nil {
			fields = append(fields, zap.Error(waitErr))
		}
		h.logger.Warn("janus browse renderer failed", fields...)
	}
	switch {
	case output.overflow.Load():
		counters.renderFailures.Add(1)
		counters.renderOverflows.Add(1)
		browseRenderError(w, http.StatusBadGateway)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		counters.renderFailures.Add(1)
		counters.renderTimeouts.Add(1)
		browseRenderError(w, http.StatusGatewayTimeout)
	case ctx.Err() != nil:
		counters.renderFailures.Add(1)
		counters.renderCancellations.Add(1)
		if generationCanceled.Load() && r.Context().Err() == nil {
			browseRenderError(w, http.StatusServiceUnavailable)
		}
	case waitErr != nil:
		counters.renderFailures.Add(1)
		browseRenderError(w, http.StatusBadGateway)
	default:
		counters.renderSuccesses.Add(1)
		w.Header().Set("Content-Type", renderer.contentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Length", fmt.Sprint(output.buffer.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(output.buffer.Bytes())
	}
}

func (h *Handler) renderContextFailure(w http.ResponseWriter, r *http.Request, ctx context.Context, generationCanceled *atomic.Bool) {
	counters := &h.app.state.browse.counters
	counters.renderFailures.Add(1)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		counters.renderTimeouts.Add(1)
		browseRenderError(w, http.StatusGatewayTimeout)
		return
	}
	counters.renderCancellations.Add(1)
	if generationCanceled.Load() && r.Context().Err() == nil {
		browseRenderError(w, http.StatusServiceUnavailable)
	}
}

func browseRendererEnvironment(inherited []string, values map[string]string) []string {
	out := make([]string, 0, len(inherited)+len(values))
	for _, item := range inherited {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "JANUS_BROWSE_") {
			continue
		}
		out = append(out, item)
	}
	var keys []string
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

func (h *Handler) renderStartFailure(w http.ResponseWriter, renderer *browseRenderer, err error) {
	h.app.state.browse.counters.renderFailures.Add(1)
	h.logger.Warn("janus browse renderer failed",
		zap.String("extension", renderer.extension),
		zap.Error(err),
	)
	browseRenderError(w, http.StatusBadGateway)
}

func browseRenderError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}
