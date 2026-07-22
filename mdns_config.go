package janus

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Cold mdns configuration (docs/20260722-034619-capability-mdns.md
// "Cold config"). Process-wide capability, the control pattern: legal
// only in the global janus block; a site-level occurrence is a parse
// error. Presence enables — there is no `mdns on|off` form, because
// nothing cascades and there is nothing for `off` to beat.

// Built-in defaults.
const mdnsDefaultName = "janus.local"

// MdnsSettings configures the process-wide mdns capability: the
// advertised `.local` identity, per-app advertising, and the plain-HTTP
// front door. The full contract is docs/20260722-034619-capability-mdns.md.
type MdnsSettings struct {
	// Name is the advertised mDNS name: exactly one label plus ".local".
	// Default: "janus.local".
	Name string `json:"name,omitempty"`

	// Canonical is the https:// origin the front door hands off to
	// (client-side probe + redirect, diagnostic mode on failure).
	// Origin only — no path, query, fragment, or userinfo; never an IP
	// literal; never a .local name. Default: unset (plain dashboard).
	Canonical string `json:"canonical,omitempty"`

	// Interfaces pins advertising to exactly these interfaces. Default:
	// unset — the live multicast interface set with the loopback and
	// IPv4 link-local block list applied.
	Interfaces []string `json:"interface,omitempty"`

	// Apps controls per-app `.local` advertising. Default: on.
	Apps *bool `json:"apps,omitempty"`

	// Listen selects the front door's mode. Unset (the default) is
	// shared mode: the front door rides inside the HTTP app's plain-HTTP
	// port server behind a normal site block (http://*.local { janus })
	// and the janus site handler decides per request. Set ("[host]:port")
	// is dedicated mode: Janus opens its own listener at that address
	// with the strict Host allowlist.
	Listen string `json:"listen,omitempty"`

	// canonicalHost is the canonical origin's hostname, derived at
	// provision for the front-door Host allowlist.
	canonicalHost string
}

// appsOn resolves the Apps knob (default on).
func (ms *MdnsSettings) appsOn() bool {
	return ms.Apps == nil || *ms.Apps
}

// shared reports whether the front door rides inside the HTTP app's
// plain-HTTP port server (no listen — the default) rather than a
// dedicated listener.
func (ms *MdnsSettings) shared() bool { return ms.Listen == "" }

// listenPort is the dedicated front-door port, derived at provision
// from the validated Listen value; 0 in shared mode.
func (ms *MdnsSettings) listenPort() int {
	_, portStr, err := net.SplitHostPort(ms.Listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(portStr)
	return n
}

// parseMdnsDirective parses one mdns directive:
//
//	mdns
//	mdns { }
//	mdns { name janus.local; canonical https://…; interface en0; apps off; listen :7680 }
//
// Hard errors per the capability contract: any argument on the mdns
// line, unknown or duplicate subdirectives, nested blocks, and every
// illegal value enumerated in the contract's parse table.
func parseMdnsDirective(d *caddyfile.Dispenser) (*MdnsSettings, error) {
	ms := &MdnsSettings{}
	if len(d.RemainingArgs()) != 0 {
		return nil, d.Err("mdns takes no arguments (presence enables; absence is off)")
	}
	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("mdns: duplicate subdirective %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "name":
			val, err := oneDirectiveArg(d, "mdns", sub)
			if err != nil {
				return nil, err
			}
			name, verr := validateMdnsName(val)
			if verr != nil {
				return nil, d.Errf("mdns name: %v", verr)
			}
			ms.Name = name
		case "canonical":
			val, err := oneDirectiveArg(d, "mdns", sub)
			if err != nil {
				return nil, err
			}
			canonical, verr := validateMdnsCanonical(val)
			if verr != nil {
				return nil, d.Errf("mdns canonical: %v", verr)
			}
			ms.Canonical = canonical
		case "interface":
			vals := d.RemainingArgs()
			if err := validateMdnsInterfaces(vals); err != nil {
				return nil, d.Errf("mdns interface: %v", err)
			}
			ms.Interfaces = vals
		case "apps":
			args := d.RemainingArgs()
			if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
				return nil, d.Err(`mdns apps: want exactly "on" or "off"`)
			}
			on := args[0] == "on"
			ms.Apps = &on
		case "listen":
			val, err := oneDirectiveArg(d, "mdns", sub)
			if err != nil {
				return nil, err
			}
			listen, verr := validateMdnsListen(val)
			if verr != nil {
				return nil, d.Errf("mdns listen: %v", verr)
			}
			ms.Listen = listen
		default:
			return nil, d.Errf("unrecognized mdns subdirective: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("mdns %s does not take a nested block", sub)
		}
	}
	return ms, nil
}

// validateMdnsName checks the advertised name: exactly one label plus
// ".local", the registry's host-label rule reused. Uppercase is
// lowercased (hostnames are case-insensitive), never rejected.
func validateMdnsName(raw string) (string, error) {
	n := strings.ToLower(raw)
	if strings.HasSuffix(n, ".") {
		return "", fmt.Errorf("want <label>.local without a trailing dot, got %q", raw)
	}
	label, ok := strings.CutSuffix(n, ".local")
	if !ok || label == "" {
		return "", fmt.Errorf("want <label>.local, got %q", raw)
	}
	if strings.Contains(label, ".") {
		return "", fmt.Errorf("want a single label plus .local (multi-label .local names are outside the mDNS host model), got %q", raw)
	}
	if len(label) > 63 || !hostLabelRE.MatchString(label) {
		return "", fmt.Errorf("label %q is not a legal hostname label (lowercase letters, digits, interior hyphens; max 63 bytes)", label)
	}
	return n, nil
}

// validateMdnsCanonical checks the canonical hand-off target: an
// https:// origin — port allowed, a bare trailing slash normalized
// away, everything else that is not an origin rejected. IP literals and
// .local names are rejected (the mode exists to hand off to a real
// HTTPS name).
func validateMdnsCanonical(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %v", raw, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("want an https:// origin, got %q", raw)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("want an https:// origin, got %q", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("origin must not carry userinfo: %q", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("origin must not carry a query: %q", raw)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("origin must not carry a fragment: %q", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("origin must not carry a path (a bare trailing slash is normalized), got %q", raw)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("origin is missing a hostname: %q", raw)
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("origin must name a hostname, not an IP literal: %q", raw)
	}
	if err := validateHostname(host); err != nil {
		return "", fmt.Errorf("origin hostname %q is not plausible", host)
	}
	if strings.HasSuffix(host, ".local") {
		return "", fmt.Errorf("origin must not be a .local name (redirecting .local to .local defeats the mode): %q", raw)
	}
	if p := u.Port(); p != "" {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("origin port %q is not a legal port", p)
		}
	}
	origin := "https://" + strings.ToLower(u.Host)
	return origin, nil
}

// validateMdnsInterfaces checks a pinned interface list: at least one
// name, no empties, no duplicates. Existence is a Start check, not a
// parse check (the config may be adapted on another machine).
func validateMdnsInterfaces(vals []string) error {
	if len(vals) == 0 {
		return fmt.Errorf("want one or more interface names")
	}
	seen := map[string]bool{}
	for _, v := range vals {
		if v == "" {
			return fmt.Errorf("interface name must not be empty")
		}
		if seen[v] {
			return fmt.Errorf("duplicate interface name %q", v)
		}
		seen[v] = true
	}
	return nil
}

// validateMdnsListen checks a dedicated front-door address:
// "[host]:port" with a port in 1–65535. An empty host is the
// dual-stack default. (An absent listen is shared mode, not an empty
// address — this validator never sees it.)
func validateMdnsListen(v string) (string, error) {
	_, portStr, err := net.SplitHostPort(v)
	if err != nil {
		return "", fmt.Errorf("want [host]:port (e.g. :80), got %q", v)
	}
	n, perr := strconv.Atoi(portStr)
	if perr != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("want a port in 1–65535, got %q", portStr)
	}
	return v, nil
}

// provisionMdns validates and normalizes the mdns settings; the native
// JSON path gets the same rejections as the Caddyfile. Never touches
// the network — binding, interface existence, and HTTP-app collision
// are Start checks.
func (a *App) provisionMdns() error {
	ms := a.Mdns
	if ms == nil {
		return nil
	}
	if ms.Name == "" {
		ms.Name = mdnsDefaultName
	}
	name, err := validateMdnsName(ms.Name)
	if err != nil {
		return fmt.Errorf("janus mdns name: %w", err)
	}
	ms.Name = name
	if ms.Canonical != "" {
		canonical, err := validateMdnsCanonical(ms.Canonical)
		if err != nil {
			return fmt.Errorf("janus mdns canonical: %w", err)
		}
		ms.Canonical = canonical
		u, _ := url.Parse(ms.Canonical)
		ms.canonicalHost = strings.ToLower(u.Hostname())
	}
	if len(ms.Interfaces) > 0 {
		if err := validateMdnsInterfaces(ms.Interfaces); err != nil {
			return fmt.Errorf("janus mdns interface: %w", err)
		}
	}
	if ms.Listen != "" {
		listen, err := validateMdnsListen(ms.Listen)
		if err != nil {
			return fmt.Errorf("janus mdns listen: %w", err)
		}
		ms.Listen = listen
	}
	return nil
}
