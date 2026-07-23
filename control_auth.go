package janus

import (
	"net/http"
)

// The auth control surface (docs/20260722-134812-capability-auth.md
// "Hot surface"). Observe and revoke, never configure: hot JSON never
// adds users, never changes ttl, never toggles the wall — the cold
// admission gate is cold (rule 2). Rides every control listener with
// the existing Bearer posture, per the control_ prefix convention.

// handleAuthState is GET /1.0/auth. Disabled answers {"enabled": false}
// — present and honest, the /1.0/mdns precedent. Enabled answers the
// wall's view: auth-enabled site patterns, the live session count, and
// the monotonic counters (never the configured usernames — the wall's
// user list is cold config, not a hot inventory).
func (a *App) handleAuthState(w http.ResponseWriter, r *http.Request) {
	sites := a.authEnabledSites()
	if len(sites) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	st := a.state.auth
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        true,
		"sites":          sites,
		"sessions":       st.sessionCount(),
		"logins":         st.logins.Load(),
		"login_failures": st.loginFailures.Load(),
		"throttled":      st.throttled.Load(),
		"signouts":       st.signouts.Load(),
		"revoked":        st.revoked.Load(),
		"reload_revoked": st.reloadRevoked.Load(),
		"expired":        st.expired.Load(),
	})
}

// handleAuthSessions is GET /1.0/auth/sessions: the live list. Each id
// is a 12-hex prefix of the HMACed store key — raw tokens are never
// retained, so no listing can leak one, and disclosing the prefix
// grants nothing (the store key is not the credential).
func (a *App) handleAuthSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": a.state.auth.sessionList(),
	})
}

// handleAuthSessionDelete is DELETE /1.0/auth/sessions/{id}: revoke one
// session by the listed id. The prefix must resolve a unique store key:
// no match → 404, ambiguous → 409.
func (a *App) handleAuthSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeAPIError(w, errBadRequest("session id is required"))
		return
	}
	found, ambiguous := a.state.auth.revokeID(id)
	if ambiguous {
		writeAPIError(w, &apiError{
			Status: http.StatusConflict,
			Msg:    "session id prefix " + id + " is ambiguous",
		})
		return
	}
	if !found {
		writeAPIError(w, &apiError{
			Status: http.StatusNotFound,
			Msg:    "unknown session id " + id,
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAuthSessionsWipe is DELETE /1.0/auth/sessions: revoke every
// session ("kick everyone" — v3's rm <dir>/*, one HTTP call).
func (a *App) handleAuthSessionsWipe(w http.ResponseWriter, r *http.Request) {
	n := a.state.auth.revokeAll()
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n})
}
