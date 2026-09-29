package main

// The webtransport relay's one host-side precondition: on macOS the pf
// anchor is the scope truth, and an anchor written before the relay
// existed blocks TCP alone, so a relay socket on UDP 443 would answer any
// source until `janus mode` rewrote it. Outside wan the edge refuses to
// start under a stale anchor rather than widen the mode silently.

import (
	"fmt"
)

// webtransportAnchorReady returns an error when the service Caddyfile
// carries the global webtransport block, the host is macOS, the mode is
// not wan, and the pf anchor does not match the stored mode.
func webtransportAnchorReady(ops fwOps, p servicePaths, st scopeState, adapt func(servicePaths) ([]byte, error)) error {
	if ops.goos != "darwin" || st.Scope == ScopeWAN {
		return nil
	}
	cfg, err := adapt(p)
	if err != nil {
		return nil // the edge itself reports an unadaptable Caddyfile, loudly
	}
	if !hasWebtransport(cfg) {
		return nil
	}
	if why := pfInstalled(ops, st); why != "" {
		return fmt.Errorf("webtransport: the relay listens on UDP 443, and the firewall rule for this mode predates it or is missing (%s); run 'janus mode %s' (as root: 'janus firewall') and start again", why, st.Scope)
	}
	return nil
}
