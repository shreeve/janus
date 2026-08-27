package janus

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/encode"
)

const ripSiteHeader = "Rip-Site"

func validateRequestPathString(p string) error {
	if p == "" || p[0] != '/' {
		return fmt.Errorf("path must start with /")
	}
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("path contains NUL or backslash")
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("path contains a %q segment", segment)
		}
	}
	return nil
}

func validatedRequestPath(r *http.Request) (string, error) {
	escaped := r.URL.EscapedPath()
	if escaped == "" {
		escaped = "/"
	}
	for i := 0; i+2 < len(escaped); i++ {
		if escaped[i] != '%' {
			continue
		}
		value, err := strconv.ParseUint(escaped[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("malformed path escape")
		}
		if value == '/' || value == '\\' {
			return "", fmt.Errorf("encoded slash or backslash")
		}
		i += 2
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return "", fmt.Errorf("malformed path escape")
	}
	if decoded != r.URL.Path {
		return "", fmt.Errorf("request path is not exactly once decoded")
	}
	if err := validateRequestPathString(decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func acceptsHTML(value string) bool {
	if strings.TrimSpace(value) == "" {
		return false
	}
	for _, item := range splitHeaderList(value) {
		mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			return false
		}
		q := 1.0
		if raw, ok := params["q"]; ok {
			q, err = strconv.ParseFloat(raw, 64)
			if err != nil || q < 0 || q > 1 {
				return false
			}
		}
		if q > 0 && (mediaType == "text/html" || mediaType == "text/*" || mediaType == "*/*") {
			return true
		}
	}
	return false
}

func splitHeaderList(value string) []string {
	var out []string
	start := 0
	quoted := false
	escaped := false
	for i, r := range value {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, value[start:i])
			start = i + 1
		}
	}
	out = append(out, value[start:])
	return out
}

// serveFiles applies the registered-file decision table. Only proxy_first
// returns handled=false; every other files path is terminal.
func (h *Handler) serveFiles(w http.ResponseWriter, r *http.Request, rec AppRecord) (handled bool, err error) {
	requestPath, pathErr := validatedRequestPath(r)
	if pathErr != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return true, nil
	}
	return h.serveFilesPath(w, r, requestPath, rec)
}

// serveFilesPath continues from the handler's one canonical path validation.
func (h *Handler) serveFilesPath(w http.ResponseWriter, r *http.Request, requestPath string, rec AppRecord) (handled bool, err error) {
	for _, prefix := range rec.Files.ProxyFirst {
		if pathPrefixMatch(requestPath, prefix) {
			return false, nil
		}
	}
	accessFactsOf(r).setClass("file")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return true, nil
	}
	// Most registrations have only one or two roots. Keep their resolved
	// descriptors on the stack; unusually large root sets spill normally.
	var rootStorage [4]activeBrowseRoot
	activeRoots := rootStorage[:0]
	if len(rec.Files.Roots) > cap(rootStorage) {
		activeRoots = make([]activeBrowseRoot, 0, len(rec.Files.Roots))
	}
	activeRoots = hotBrowseRoots(rec.Files.Roots, rec.siteValue, activeRoots)
	if h.serveBrowseRootsPrecompressed(w, r, requestPath, activeRoots, rec.Files.Shell, true, h.filesPrecompressed()) {
		return true, nil
	}
	http.NotFound(w, r)
	return true, nil
}

func serveAbsoluteFile(w http.ResponseWriter, r *http.Request, name string, precompressed []string) bool {
	root, err := os.OpenRoot(filepath.Dir(name))
	if err != nil {
		return false
	}
	defer root.Close()
	file, err := root.Open(filepath.Base(name))
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	serveOpenedFileFromRoot(w, r, root, file, info, filepath.Base(name), name, filesCacheRevalidate, precompressed)
	return true
}

func serveOpenedFile(w http.ResponseWriter, r *http.Request, file *os.File, info os.FileInfo, name, cache string) {
	serveOpenedFileFromRoot(w, r, nil, file, info, "", name, cache, nil)
}

func serveOpenedFileFromRoot(w http.ResponseWriter, r *http.Request, root *os.Root, file *os.File, info os.FileInfo, relative, name, cache string, precompressed []string) {
	selectedFile, selectedInfo, encoding := file, info, ""
	if len(precompressed) > 0 {
		addVaryAcceptEncoding(w.Header())
		for _, candidate := range acceptedFileEncodings(r, precompressed) {
			if candidate == "identity" {
				break
			}
			suffix, ok := filesPrecompressedSuffix[candidate]
			if !ok || root == nil {
				continue
			}
			sidecar, err := root.Open(relative + suffix)
			if err != nil {
				continue
			}
			sidecarInfo, err := sidecar.Stat()
			if err != nil || !sidecarInfo.Mode().IsRegular() {
				sidecar.Close()
				continue
			}
			defer sidecar.Close()
			selectedFile, selectedInfo, encoding = sidecar, sidecarInfo, candidate
			break
		}
	}

	ext := strings.ToLower(filepath.Ext(name))
	switch cache {
	case filesCacheNever:
		w.Header().Set("Cache-Control", "no-store")
	case filesCacheForever:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "no-cache")
	}
	switch ext {
	case ".rip":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".html", ".htm":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	default:
		if contentType := mime.TypeByExtension(ext); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
	}
	if encoding != "" {
		w.Header().Set("Content-Encoding", encoding)
		w.Header().Del("Content-Length")
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", strconv.FormatInt(selectedInfo.Size(), 10))
		}
		w.Header().Set("ETag", fmt.Sprintf(`W/"%x-%x-%s"`, selectedInfo.ModTime().UnixNano(), selectedInfo.Size(), encoding))
	} else {
		w.Header().Set("ETag", fmt.Sprintf(`W/"%x-%x"`, selectedInfo.ModTime().UnixNano(), selectedInfo.Size()))
	}
	http.ServeContent(w, r, name, selectedInfo.ModTime(), selectedFile)
}

func addVaryAcceptEncoding(header http.Header) {
	for _, value := range header.Values("Vary") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Accept-Encoding") {
				return
			}
		}
	}
	header.Add("Vary", "Accept-Encoding")
}

// acceptedFileEncodings preserves Caddy's q-value and configured-order
// ranking. Caddy exposes wildcards as the literal "*"; expand that position
// into configured formats while keeping every explicitly named coding,
// including q=0 exclusions, out of the wildcard set.
func acceptedFileEncodings(r *http.Request, configured []string) []string {
	explicit := make(map[string]bool)
	for _, item := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, _, _ := strings.Cut(item, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name != "" && name != "*" {
			explicit[name] = true
		}
	}
	enabled := make(map[string]bool, len(configured))
	for _, format := range configured {
		enabled[format] = true
	}
	var out []string
	seen := make(map[string]bool, len(configured)+1)
	appendCandidate := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, candidate := range encode.AcceptedEncodings(r, configured) {
		switch {
		case candidate == "identity":
			appendCandidate(candidate)
		case candidate == "*":
			for _, format := range configured {
				if !explicit[format] {
					appendCandidate(format)
				}
			}
		case enabled[candidate]:
			appendCandidate(candidate)
		}
	}
	return out
}
