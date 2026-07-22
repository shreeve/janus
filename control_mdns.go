package janus

import (
	"fmt"
	"net/http"
)

// The mdns control surface (docs/20260722-034619-capability-mdns.md
// "Registry, control-surface, and repo deltas"). GET /1.0/mdns rides
// every control listener with the existing Bearer posture — the
// acceptance oracle (multicast is not CI-assertable; advertiser state
// is) and the operator's view of the advertiser.

// handleMdnsState is GET /1.0/mdns. Disabled answers {"enabled": false}
// — present and honest, like /1.0/cache with no cache-enabled sites.
// Enabled answers the advertiser view: configured and effective names,
// the front door's mode and address (shared mode names the HTTP port
// the door rides inside; dedicated mode names its own listener), every
// advertised entry with its pinned state (probing | announced |
// renamed), the skipped-hosts gauge, and the monotonic
// announces/withdraws counters the reload no-flap acceptance case
// reads.
func (a *App) handleMdnsState(w http.ResponseWriter, r *http.Request) {
	if a.Mdns == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	ms := a.Mdns
	snap := a.state.mdns.snapshot(ms.Name)
	advertised := snap.entries
	if advertised == nil {
		advertised = []mdnsAdvertisedEntry{}
	}
	mode, frontDoor := "dedicated", ms.Listen
	if ms.shared() {
		mode, frontDoor = "shared", fmt.Sprintf(":%d", a.mdnsSharedPort)
	}
	body := map[string]any{
		"enabled":        true,
		"name":           ms.Name,
		"effective_name": snap.effectiveName,
		"mode":           mode,
		"front_door":     frontDoor,
		"advertised":     advertised,
		"skipped_hosts":  snap.skipped,
		"announces":      snap.announces,
		"withdraws":      snap.withdraws,
	}
	if ms.Canonical != "" {
		body["canonical"] = ms.Canonical
	}
	if len(ms.Interfaces) > 0 {
		body["interfaces"] = ms.Interfaces
	}
	writeJSON(w, http.StatusOK, body)
}
