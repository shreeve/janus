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
	for _, prefix := range rec.Files.ProxyFirst {
		if pathPrefixMatch(requestPath, prefix) {
			return false, nil
		}
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return true, nil
	}
	relative := strings.TrimPrefix(requestPath, "/")
	if relative == "" {
		relative = "."
	}
	for _, configuredRoot := range rec.Files.Roots {
		rootPath := strings.ReplaceAll(configuredRoot.Path, "{site}", rec.siteValue)
		if serveRootedFile(w, r, rootPath, relative, requestPath, configuredRoot.Class) {
			return true, nil
		}
	}
	if acceptsHTML(r.Header.Get("Accept")) && serveAbsoluteFile(w, r, rec.Files.Shell) {
		return true, nil
	}
	http.NotFound(w, r)
	return true, nil
}

func serveRootedFile(w http.ResponseWriter, r *http.Request, rootPath, relative, displayName, class string) bool {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return false
	}
	defer root.Close()
	file, err := root.Open(relative)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	serveOpenedFile(w, r, file, info, displayName, class)
	return true
}

func serveAbsoluteFile(w http.ResponseWriter, r *http.Request, name string) bool {
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
	serveOpenedFile(w, r, file, info, name, filesClassGenerated)
	return true
}

func serveOpenedFile(w http.ResponseWriter, r *http.Request, file *os.File, info os.FileInfo, name, class string) {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case class == filesClassLive && ext == ".rip":
		w.Header().Set("Cache-Control", "no-store")
	case class == filesClassVersioned:
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
	w.Header().Set("ETag", fmt.Sprintf(`W/"%x-%x"`, info.ModTime().UnixNano(), info.Size()))
	http.ServeContent(w, r, name, info.ModTime(), file)
}
