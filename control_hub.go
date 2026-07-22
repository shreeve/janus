package janus

import (
	"io"
	"net/http"
)

// The publish plane and hub observability
// (docs/20260720-162350-hub-design.md "The publish plane",
// "Observability"). All three surfaces ride the existing control
// listeners and inherit their Bearer-token posture. Within the current
// flat control-listener trust domain, publish is a cross-app capability;
// host-claim scoping will add an explicit caller-owns-app check here.

// handleHubPublish is POST /1.0/apps/{id}/hub/publish — the hot-plane
// injection point. It touches only hub state: never upstreams, never the
// doorbell. A publish while the app is mid-reload delivers normally.
func (a *App) handleHubPublish(w http.ResponseWriter, r *http.Request) {
	rec, err := a.appsRegistry().get(r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if !a.hubEnabledAnywhere(rec) {
		// Publishing into a hub that cannot have members is a caller bug,
		// said loudly.
		writeAPIError(w, &apiError{
			Status: http.StatusConflict,
			Msg:    "hub is not enabled for any site of this app",
		})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, errBadRequest("reading body: %v", err))
		return
	}
	objs, verr := parseHubFrame(body, hubPlanePublish)
	if verr != nil {
		writeAPIError(w, errBadRequest("%s", verr.msg))
		return
	}

	hub := a.hubs.getOrCreate(rec.ID, a.appsReg.exists)
	if hub == nil {
		// The registration died between the get above and here.
		writeAPIError(w, errUnknownApp(rec.ID))
		return
	}
	out, verr := hub.execute(objs, hubPlanePublish, nil)
	if verr != nil {
		writeAPIError(w, errBadRequest("%s", verr.msg))
		return
	}
	hub.ctr.publishes.Add(1)
	writeJSON(w, http.StatusOK, map[string]int{
		"objects":         len(objs),
		"deliveries":      out.deliveries,
		"unknown_targets": out.unknownTargets,
	})
}

// handleHubSnapshot is GET /1.0/apps/{id}/hub — the tenant's resync
// instrument: connection count, channel names with member counts, and
// per-connection channel lists keyed by opaque snapshot handles (never
// raw connection ids, never usable as wire targets).
func (a *App) handleHubSnapshot(w http.ResponseWriter, r *http.Request) {
	rec, err := a.appsRegistry().get(r.PathValue("id"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	hub := a.hubs.get(rec.ID)
	if hub == nil {
		writeJSON(w, http.StatusOK, hubMembershipSnapshot{
			Channels:    map[string]int{},
			Connections: map[string][]string{},
		})
		return
	}
	writeJSON(w, http.StatusOK, hub.membershipSnapshot())
}

// handleHubStats is GET /1.0/hub — process totals and per-app breakdown,
// always on. Atomics summed at read time; a tight scrape loop can never
// degrade the hot path.
func (a *App) handleHubStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.hubs.stats())
}
