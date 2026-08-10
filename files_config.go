package janus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"path"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// SitePolicy declares one directory-gated host pattern and its exact aliases.
type SitePolicy struct {
	Host    string            `json:"host"`
	Dir     string            `json:"dir"`
	Aliases map[string]string `json:"aliases,omitempty"`
}

// FilesPolicy declares ordered static roots, worker-first prefixes, and the
// independent SPA shell.
type FilesPolicy struct {
	Roots      []FilesRoot `json:"roots"`
	ProxyFirst []string    `json:"proxy_first,omitempty"`
	Shell      string      `json:"shell,omitempty"`
}

// FilesRoot is one ordered root with a finite cache policy.
type FilesRoot struct {
	Path   string `json:"path"`
	Cache  string `json:"cache,omitempty"`
	Browse bool   `json:"browse"`
}

func (r *FilesRoot) UnmarshalJSON(data []byte) error {
	var raw struct {
		Path   json.RawMessage `json:"path"`
		Cache  json.RawMessage `json:"cache"`
		Browse json.RawMessage `json:"browse"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return err
	}
	if len(raw.Path) != 0 {
		if bytes.Equal(bytes.TrimSpace(raw.Path), []byte("null")) || json.Unmarshal(raw.Path, &r.Path) != nil {
			return fmt.Errorf("files root path must be a string")
		}
	}
	if len(raw.Cache) != 0 {
		if bytes.Equal(bytes.TrimSpace(raw.Cache), []byte("null")) || json.Unmarshal(raw.Cache, &r.Cache) != nil {
			return fmt.Errorf("files root cache must be a string")
		}
	}
	if len(raw.Browse) != 0 {
		if bytes.Equal(bytes.TrimSpace(raw.Browse), []byte("null")) || json.Unmarshal(raw.Browse, &r.Browse) != nil {
			return fmt.Errorf("files root browse must be a boolean")
		}
	}
	return nil
}

const (
	filesCacheNever      = "never"
	filesCacheRevalidate = "revalidate"
	filesCacheForever    = "forever"
)

var filesDefaultPrecompressed = []string{"br", "zstd", "gzip"}

var filesPrecompressedSuffix = map[string]string{
	"br":   ".br",
	"zstd": ".zst",
	"gzip": ".gz",
}

func validateFilesPrecompressed(files *bool, formats []string) error {
	if len(formats) == 0 {
		return nil
	}
	if files == nil || !*files {
		return fmt.Errorf("janus files: files_precompressed requires files on")
	}
	seen := make(map[string]bool, len(formats))
	for _, format := range formats {
		if _, ok := filesPrecompressedSuffix[format]; !ok {
			return fmt.Errorf("janus files: unknown precompressed format %q (want br, zstd, or gzip)", format)
		}
		if seen[format] {
			return fmt.Errorf("janus files: duplicate precompressed format %q", format)
		}
		seen[format] = true
	}
	return nil
}

func parseFilesDirective(d *caddyfile.Dispenser, global bool) (*bool, []string, error) {
	on, err := parseOnOff(d.RemainingArgs())
	if err != nil {
		return nil, nil, d.Errf("files: %v", err)
	}
	var precompressed []string
	seenPrecompressed := false
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if !global {
			return nil, nil, d.Err("files settings are process-wide; set them in the global janus block only")
		}
		if !on {
			return nil, nil, d.Err("files off does not take a block (a block on an off switch is a contradiction)")
		}
		switch d.Val() {
		case "precompressed":
			if seenPrecompressed {
				return nil, nil, d.Err("duplicate precompressed directive in the same files block")
			}
			seenPrecompressed = true
			formats := d.RemainingArgs()
			if len(formats) == 0 {
				formats = filesDefaultPrecompressed
			}
			seenFormats := make(map[string]bool, len(formats))
			for _, format := range formats {
				if _, ok := filesPrecompressedSuffix[format]; !ok {
					return nil, nil, d.Errf("files precompressed: unknown format %q (want br, zstd, or gzip)", format)
				}
				if seenFormats[format] {
					return nil, nil, d.Errf("files precompressed: duplicate format %q", format)
				}
				seenFormats[format] = true
			}
			precompressed = append([]string(nil), formats...)
			if d.NextBlock(d.Nesting()) {
				return nil, nil, d.Err("files precompressed does not take a nested block")
			}
		default:
			return nil, nil, d.Errf("unrecognized files subdirective: %s", d.Val())
		}
	}
	return &on, precompressed, nil
}

func validateConfiguredPath(p, field string, allowSite bool) error {
	if p == "" {
		return errBadRequest("%s is required", field)
	}
	if !strings.HasPrefix(p, "/") {
		return errBadRequest("%s %q must be an absolute Unix path", field, p)
	}
	if strings.ContainsAny(p, "\x00\\") {
		return errBadRequest("%s %q contains NUL or backslash", field, p)
	}
	if strings.Contains(p, "//") {
		return errBadRequest("%s %q contains a repeated separator", field, p)
	}
	if path.Clean(p) != p {
		return errBadRequest("%s %q is not clean", field, p)
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "." || part == ".." {
			return errBadRequest("%s %q contains a %q segment", field, p, part)
		}
	}
	count := strings.Count(p, "{site}")
	if count > 0 && (!allowSite || count != 1) {
		return errBadRequest("%s %q has an unsupported {site} template", field, p)
	}
	if strings.Contains(strings.ReplaceAll(p, "{site}", ""), "{") ||
		strings.Contains(strings.ReplaceAll(p, "{site}", ""), "}") {
		return errBadRequest("%s %q has an unsupported template", field, p)
	}
	return nil
}

func validateSiteLabel(label, field string) error {
	if label == "common" {
		return errBadRequest("%s uses reserved site label %q", field, label)
	}
	if len(label) > 63 || !hostLabelRE.MatchString(label) {
		return errBadRequest("%s %q is not a lowercase DNS label", field, label)
	}
	return nil
}

func normalizeSitePolicy(in *SitePolicy) (*SitePolicy, string, error) {
	if in == nil {
		return nil, "", nil
	}
	if in.Host == "" || strings.Count(in.Host, "{site}") != 1 ||
		!strings.HasPrefix(in.Host, "{site}.") {
		return nil, "", errBadRequest(`site.host %q must have the form "{site}.<hostname>"`, in.Host)
	}
	suffix := strings.TrimPrefix(in.Host, "{site}.")
	if strings.Contains(suffix, "{") || strings.Contains(suffix, "}") {
		return nil, "", errBadRequest("site.host %q contains an unsupported template", in.Host)
	}
	suffix = strings.ToLower(suffix)
	if err := validateHostname(suffix); err != nil {
		return nil, "", errBadRequest("invalid site.host %q: %v", in.Host, err)
	}
	if err := validateConfiguredPath(in.Dir, "site.dir", false); err != nil {
		return nil, "", err
	}
	out := &SitePolicy{Host: "{site}." + suffix, Dir: in.Dir}
	if in.Aliases != nil {
		out.Aliases = make(map[string]string, len(in.Aliases))
	}
	for rawHost, rawSite := range in.Aliases {
		host := strings.ToLower(rawHost)
		if err := validateHostname(host); err != nil {
			return nil, "", errBadRequest("invalid site alias host %q: %v", rawHost, err)
		}
		if net.ParseIP(host) != nil {
			return nil, "", errBadRequest("site alias %q must be a DNS hostname, not an IP literal", host)
		}
		// Beneath the pattern, an alias earns its keep only by REMAPPING:
		// {site} extraction would already resolve ola.<suffix> to "ola",
		// so a self-alias is noise — but local.<suffix> -> "ola" changes
		// the answer and is the supported spelling for a dev host that
		// rides the wildcard certificate.
		if strings.HasSuffix(host, "."+suffix) && strings.TrimSuffix(host, "."+suffix) == strings.ToLower(rawSite) {
			return nil, "", errBadRequest("site alias %q is redundant beneath pattern %q", host, out.Host)
		}
		if err := validateSiteLabel(rawSite, fmt.Sprintf("site alias %q", host)); err != nil {
			return nil, "", err
		}
		out.Aliases[host] = rawSite
	}
	return out, suffix, nil
}

func normalizeFilesPolicy(in *FilesPolicy, hasSite bool) (*FilesPolicy, error) {
	if in == nil {
		return nil, nil
	}
	if len(in.Roots) == 0 {
		return nil, errBadRequest("files.roots is required and must not be empty")
	}
	if in.Shell == "" {
		for _, root := range in.Roots {
			if !root.Browse {
				return nil, errBadRequest("files.shell may be omitted only when every root has browse true")
			}
		}
		if len(in.ProxyFirst) != 0 {
			return nil, errBadRequest("files.shell may be omitted only when proxy_first is empty")
		}
	}
	out := &FilesPolicy{Roots: make([]FilesRoot, 0, len(in.Roots)), Shell: in.Shell}
	seenRoots := map[string]bool{}
	for _, root := range in.Roots {
		if err := validateConfiguredPath(root.Path, "files.roots.path", hasSite); err != nil {
			return nil, err
		}
		if root.Cache == "" {
			root.Cache = filesCacheRevalidate
		}
		switch root.Cache {
		case filesCacheNever, filesCacheRevalidate, filesCacheForever:
		default:
			return nil, errBadRequest("files root %q has invalid cache %q (want never, revalidate, or forever)", root.Path, root.Cache)
		}
		if strings.Contains(root.Path, "{site}") && !hasSite {
			return nil, errBadRequest("files root %q uses {site} without site", root.Path)
		}
		if seenRoots[root.Path] {
			return nil, errBadRequest("duplicate files root %q", root.Path)
		}
		seenRoots[root.Path] = true
		out.Roots = append(out.Roots, root)
	}
	if in.Shell != "" {
		if err := validateConfiguredPath(in.Shell, "files.shell", false); err != nil {
			return nil, err
		}
	}
	for _, prefix := range in.ProxyFirst {
		if err := validateRequestPathString(prefix); err != nil {
			return nil, errBadRequest("files.proxy_first %q: %v", prefix, err)
		}
		if prefix != "/" && strings.HasSuffix(prefix, "/") {
			return nil, errBadRequest("files.proxy_first %q is not normalized", prefix)
		}
		if path.Clean(prefix) != prefix {
			return nil, errBadRequest("files.proxy_first %q is not normalized", prefix)
		}
		for _, prior := range out.ProxyFirst {
			if pathPrefixMatch(prefix, prior) || pathPrefixMatch(prior, prefix) {
				return nil, errBadRequest("files.proxy_first entries %q and %q overlap", prior, prefix)
			}
		}
		out.ProxyFirst = append(out.ProxyFirst, prefix)
	}
	return out, nil
}

func cloneSitePolicy(in *SitePolicy) *SitePolicy {
	if in == nil {
		return nil
	}
	out := *in
	if in.Aliases != nil {
		out.Aliases = make(map[string]string, len(in.Aliases))
		for host, site := range in.Aliases {
			out.Aliases[host] = site
		}
	}
	return &out
}

func cloneFilesPolicy(in *FilesPolicy) *FilesPolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.Roots = append([]FilesRoot(nil), in.Roots...)
	out.ProxyFirst = append([]string(nil), in.ProxyFirst...)
	return &out
}

func pathPrefixMatch(requestPath, prefix string) bool {
	return prefix == "/" || requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}
