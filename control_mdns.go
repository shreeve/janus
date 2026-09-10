package janus

import (
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Published after listener setup. Readers can arrive while Start is still
// running; neither listener fields nor growing route slices are read live.
type mdnsFrontDoorInfo struct {
	address string
	routes  []caddyhttp.RouteList
}

// The mdns control surface (docs/20260722-034619-capability-mdns.md
// "Registry, control-surface, and repo deltas"). GET /1.0/mdns rides
// every control listener with the existing Bearer posture — the
// acceptance oracle (multicast is not CI-assertable; advertiser state
// is) and the operator's view of the advertiser.

// handleMdnsState is GET /1.0/mdns. Disabled answers enabled:false and
// empty URLs — present and honest even when mDNS is disabled.
// Enabled answers the advertiser view: configured and effective names,
// the front door's mode and address (shared mode names the HTTP port
// the door rides inside; dedicated mode names its own listener), every
// advertised entry with its pinned state (probing | announced |
// renamed), the skipped-hosts gauge, and the monotonic
// announces/withdraws counters the reload no-flap acceptance case
// reads.
func (a *App) handleMdnsState(w http.ResponseWriter, r *http.Request) {
	if a.Mdns == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "dashboard_url": "", "trust_url": ""})
		return
	}
	ms := a.Mdns
	snap := a.state.mdns.snapshot(ms.Name)
	advertised := snap.entries
	if advertised == nil {
		advertised = []mdnsAdvertisedEntry{}
	}
	mode, frontDoor := "dedicated", ms.Listen
	address := ""
	ownsName := !ms.shared()
	if ms.shared() {
		mode, frontDoor = "shared", ""
	}
	if door := a.mdnsDoor.Load(); door != nil {
		address = door.address
		if ms.shared() {
			frontDoor = address
		}
		for _, routes := range door.routes {
			if mdnsRouteOwner(routes, snap.effectiveName) == mdnsRouteOwned {
				ownsName = true
				break
			}
		}
	}
	dashboard, trust := mdnsFrontDoorURLs(ms, snap.effectiveName, address, ExposureScope(), ownsName)
	body := map[string]any{
		"enabled":        true,
		"name":           ms.Name,
		"effective_name": snap.effectiveName,
		"mode":           mode,
		"front_door":     frontDoor,
		"dashboard_url":  dashboard,
		"trust_url":      trust,
		"advertised":     advertised,
		"skipped_hosts":  snap.skipped,
		"announces":      snap.announces,
		"withdraws":      snap.withdraws,
	}
	if wanExposure() {
		body["exposure"] = "wan" // nothing is announced or served on local names
	}
	if ms.Canonical != "" {
		body["canonical"] = ms.Canonical
	}
	if len(ms.Interfaces) > 0 {
		body["interfaces"] = ms.Interfaces
	}
	writeJSON(w, http.StatusOK, body)
}

// URLs are resolved once by Janus; clients need not reproduce listener,
// exposure, conflict-renaming, or canonical-handoff rules.
func mdnsFrontDoorURLs(ms *MdnsSettings, name, address, scope string, ownsName bool) (dashboard, trust string) {
	if ms == nil {
		return "", ""
	}
	if ms.Canonical != "" {
		dashboard = strings.TrimRight(ms.Canonical, "/") + "/"
	}
	host, port, err := net.SplitHostPort(address)
	n, parseErr := strconv.Atoi(port)
	if err != nil || parseErr != nil || n < 1 || n > 65535 || strings.Contains(host, "%") {
		return dashboard, ""
	}
	origin := func(host string) string {
		authority := net.JoinHostPort(host, port)
		if port == "80" {
			authority = strings.TrimSuffix(authority, ":80")
		}
		return (&url.URL{Scheme: "http", Host: authority, Path: "/"}).String()
	}
	lan := scope != "localhost" && scope != "wan" && ownsName && name != ""
	if ms.shared() {
		if lan {
			if dashboard == "" {
				dashboard = origin(name)
			}
			trust = origin(name) + "trust"
		}
		return dashboard, trust
	}
	ip := net.ParseIP(host)
	if host == "localhost" || (ip != nil && ip.IsLoopback()) {
		lan = false
	}
	switch host {
	case "", "0.0.0.0", "localhost":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	default:
		if ip == nil {
			return dashboard, ""
		}
	}
	if dashboard == "" {
		dashboard = origin(host)
	}
	if lan {
		trust = origin(name) + "trust"
	}
	return dashboard, trust
}

// handleMdnsStatus is GET /1.0/mdns/status: the front door's status
// snapshot (registry shape, worker health, heartbeat ages, hub counters,
// launch links; socket paths redacted) on the control surface, for an
// operator or a control plane that owns the announced name itself and
// so never sees the front door. 404 when mDNS is off.
func (a *App) handleMdnsStatus(w http.ResponseWriter, r *http.Request) {
	if a.Mdns == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "mdns is off"})
		return
	}
	writeJSON(w, http.StatusOK, a.mdnsStatusSnapshot())
}
