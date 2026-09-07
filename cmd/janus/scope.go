package main

// Exposure state: the mode the service edge runs in, kept as scope.json under
// the state directory. It is the one source of truth for the front-door bind
// — every verb that runs, reloads, or validates the service Caddyfile derives
// JANUS_BIND from it in-process, so the Caddyfile's `default_bind
// {$JANUS_BIND}` always says what the mode says. Only autostart and mode
// write it, and always as the edge's own user. An absent file is
// localhost, the fail-closed default; a file that does not parse is refused,
// never repaired.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// bindEnvKey is the environment variable the service Caddyfile binds through.
const bindEnvKey = "JANUS_BIND"

// scopeState is scope.json. lan is one interface's private IPv4 address and
// the on-link subnet it sits in (what the macOS firewall admits). The
// interface is the default route's unless the operator pinned one.
type scopeState struct {
	Scope     Scope  `json:"scope"`
	Interface string `json:"interface,omitempty"`
	Pinned    bool   `json:"interface_pinned,omitempty"` // named with --interface; kept until --interface auto
	LANV4     string `json:"lan_v4,omitempty"`
	OnLinkV4  string `json:"onlink_v4,omitempty"`
}

// lan is the lan address, zero when the scope has none.
func (s scopeState) lan() netip.Addr {
	a, _ := netip.ParseAddr(s.LANV4)
	return a
}

// onlink is the lan subnet, zero when the scope has none.
func (s scopeState) onlink() netip.Prefix {
	p, _ := netip.ParsePrefix(s.OnLinkV4)
	return p
}

// bind is the default_bind list for this state on an OS.
func (s scopeState) bind(goos string) ([]string, error) {
	return BindPlan(s.Scope, goos, s.lan())
}

// validate rejects a state that cannot be realized: an unknown scope, lan
// without an interface or without a usable address, an on-link prefix that
// is not a private subnet containing the address, or lan fields on a scope
// that has none.
func (s scopeState) validate() error {
	switch s.Scope {
	case ScopeLocalhost, ScopeLAN, ScopeWAN:
	default:
		return fmt.Errorf("scope %q is not one of localhost, lan, wan", s.Scope)
	}
	if s.Scope != ScopeLAN {
		if s.Interface != "" || s.Pinned || s.LANV4 != "" || s.OnLinkV4 != "" {
			return fmt.Errorf("scope %s carries interface fields, which only lan has", s.Scope)
		}
		return nil
	}
	if s.Interface == "" {
		return errors.New("lan scope names no interface")
	}
	a, err := netip.ParseAddr(s.LANV4)
	if err != nil {
		return fmt.Errorf("lan_v4 %q: %v", s.LANV4, err)
	}
	if _, err := PlanListeners(ScopeLAN, a); err != nil {
		return err
	}
	p, err := netip.ParsePrefix(s.OnLinkV4)
	if err != nil {
		return fmt.Errorf("onlink_v4 %q: %v", s.OnLinkV4, err)
	}
	if p != p.Masked() || p.Bits() >= 32 || !p.Contains(a) || !withinRFC1918(p) {
		return fmt.Errorf("onlink_v4 %s is not a private subnet containing lan_v4 %s", p, a)
	}
	return nil
}

// withinRFC1918 reports whether a prefix lies entirely inside one private
// block — the most a lan on-link subnet may admit.
func withinRFC1918(p netip.Prefix) bool {
	for _, block := range rfc1918 {
		if block.Contains(p.Addr()) && p.Bits() >= block.Bits() {
			return true
		}
	}
	return false
}

// readScope loads scope.json. Absent means localhost.
func readScope(p servicePaths) (scopeState, error) {
	b, err := os.ReadFile(p.scope)
	if errors.Is(err, os.ErrNotExist) {
		return scopeState{Scope: DefaultScope}, nil
	}
	if err != nil {
		return scopeState{}, err
	}
	var s scopeState
	if err := decodeOneObject(b, &s); err != nil {
		return scopeState{}, fmt.Errorf("%s does not parse: %v (fix it, or remove it for localhost)", p.scope, err)
	}
	if err := s.validate(); err != nil {
		return scopeState{}, fmt.Errorf("%s: %v (fix it, or remove it for localhost)", p.scope, err)
	}
	return s, nil
}

// decodeOneObject decodes exactly one JSON object with no unknown fields,
// no repeated keys, and nothing after it.
func decodeOneObject(b []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	keys := json.NewDecoder(strings.NewReader(string(b)))
	if tok, err := keys.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("not a JSON object")
	}
	seen := map[string]bool{}
	for keys.More() {
		tok, err := keys.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("key %q appears twice", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := keys.Decode(&skip); err != nil {
			return err
		}
	}
	return nil
}

// writeScope stores the state, validated first, by writing beside the file
// and renaming over it.
func writeScope(p servicePaths, s scopeState) error {
	if err := s.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.state, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p.scope, append(b, '\n'), 0o644)
}

// setBindEnv derives JANUS_BIND from scope.json for this OS and puts it in
// the process environment, where the Caddyfile adapter reads {$JANUS_BIND}.
// Every path that adapts the service Caddyfile — run, reload, validate —
// goes through here, so the bind can only ever be what the scope says.
func setBindEnv(p servicePaths) (scopeState, error) {
	s, err := readScope(p)
	if err != nil {
		return s, err
	}
	bind, err := s.bind(runtime.GOOS)
	if err != nil {
		return s, fmt.Errorf("%s: %v", p.scope, err)
	}
	return s, os.Setenv(bindEnvKey, strings.Join(bind, " "))
}

// loadEnvFile puts KEY=value lines from the service env file into the
// process environment, in Caddy's envfile format (comments, `export `,
// quotes), for the verbs that adapt the Caddyfile in-process without Caddy's
// own --envfile. A missing file is nothing to load. JANUS_BIND is refused
// there: the bind comes from the mode, nowhere else.
func loadEnvFile(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for n, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s line %d: not KEY=value", path, n+1)
		}
		key = strings.TrimPrefix(key, "export ")
		if key == "" || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("%s line %d: bad key %q", path, n+1, key)
		}
		if key == bindEnvKey {
			return fmt.Errorf("%s line %d: %s is the exposure mode's; 'janus mode <scope>' sets it", path, n+1, bindEnvKey)
		}
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimRight(val[:i], " \t")
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	return nil
}

// scopeFile is where scope.json lives for a set of paths.
func scopeFile(p servicePaths) string { return filepath.Join(p.state, "scope.json") }
