package janus

// GET /1.0/vista: each route's sessions, caps, and counters. No token,
// session id, client address, or user is published.

import "net/http"

func (a *App) handleVistaState(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"routes": map[string]vistaRouteStatus{}}
	if a.state == nil || a.state.vista == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"routes": a.state.vista.status()})
}

func (p *vistaPool) status() map[string]vistaRouteStatus {
	p.mu.Lock()
	routes := make([]*vistaRouteState, 0, len(p.routes))
	for _, rs := range p.routes {
		routes = append(routes, rs)
	}
	p.mu.Unlock()
	out := make(map[string]vistaRouteStatus, len(routes))
	for _, rs := range routes {
		out[rs.key] = rs.status(rs.lim.Load())
	}
	return out
}
