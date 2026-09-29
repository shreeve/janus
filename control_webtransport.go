package janus

// GET /1.0/webtransport: the relay's counters. No remote address is
// published; the surface is counters, not a session directory.

import (
	"net/http"
	"sort"
)

type wtRouteStatus struct {
	Target             string   `json:"target"`
	Origin             []string `json:"origin"`
	Sessions           int64    `json:"sessions"`
	MaxSessions        int      `json:"max_sessions"`
	Gated              bool     `json:"gated"`
	Accepted           int64    `json:"accepted"`
	RefusedOrigin      int64    `json:"refused_origin"`
	RefusedCap         int64    `json:"refused_cap"`
	RefusedUpstream    int64    `json:"refused_upstream"`
	RefusedSettings    int64    `json:"refused_settings"`
	RefusedTicket      int64    `json:"refused_ticket"`
	DatagramsIn        int64    `json:"datagrams_in"`
	DatagramsOut       int64    `json:"datagrams_out"`
	BytesIn            int64    `json:"bytes_in"`
	BytesOut           int64    `json:"bytes_out"`
	DroppedQueue       int64    `json:"dropped_queue"`
	DroppedOversizeIn  int64    `json:"dropped_oversize_in"`
	DroppedOversizeOut int64    `json:"dropped_oversize_out"`
	UpstreamRefused    int64    `json:"upstream_refused"`
	StreamsReset       int64    `json:"streams_reset"`
	Panics             int64    `json:"panics"`
}

type wtStatus struct {
	Enabled         bool                     `json:"enabled"`
	Listen          []string                 `json:"listen"`
	Sessions        int64                    `json:"sessions"`
	MaxSessions     int                      `json:"max_sessions"`
	MaxDatagram     int                      `json:"max_datagram"`
	IdleTimeout     string                   `json:"idle_timeout"`
	Routes          map[string]wtRouteStatus `json:"routes"`
	RefusedHost     int64                    `json:"refused_host"`
	RefusedPath     int64                    `json:"refused_path"`
	RefusedFronting int64                    `json:"refused_fronting"`
	RefusedMethod   int64                    `json:"refused_method"`
	RetriesSent     int64                    `json:"retries_sent"`
}

func (a *App) handleWebtransportState(w http.ResponseWriter, r *http.Request) {
	if a.Webtransport == nil || a.state == nil || a.state.webtransport == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, a.state.webtransport.status())
}

func (r *wtRelay) status() wtStatus {
	table := r.live.Load()
	st := wtStatus{
		Enabled: true, Listen: []string{}, Sessions: r.sessionN.Load(),
		MaxSessions: table.maxSessions, MaxDatagram: table.maxDatagram, IdleTimeout: table.idleTimeout.String(),
		Routes:      map[string]wtRouteStatus{},
		RefusedHost: r.refusedHost.Load(), RefusedPath: r.refusedPath.Load(),
		RefusedFronting: r.refusedFronting.Load(), RefusedMethod: r.refusedMethod.Load(),
		RetriesSent: r.retriesSent.Load(),
	}
	for _, l := range table.listen {
		st.Listen = append(st.Listen, l.String())
	}
	sort.Strings(st.Listen)
	for key, rt := range table.routes {
		rc := rt.config()
		origin := rc.origin
		if len(origin) == 0 {
			origin = []string{"same"}
		}
		c := &rt.counters
		st.Routes[key] = wtRouteStatus{
			Target: "udp/" + rt.target.String(), Origin: origin, Sessions: rt.sessions.Load(), MaxSessions: rc.maxSessions, Gated: rc.gated,
			Accepted: c.accepted.Load(), RefusedOrigin: c.refusedOrigin.Load(), RefusedCap: c.refusedCap.Load(),
			RefusedUpstream: c.refusedUpstream.Load(), RefusedSettings: c.refusedSettings.Load(), RefusedTicket: c.refusedTicket.Load(),
			DatagramsIn: c.datagramsIn.Load(), DatagramsOut: c.datagramsOut.Load(), BytesIn: c.bytesIn.Load(), BytesOut: c.bytesOut.Load(),
			DroppedQueue: c.droppedQueue.Load(), DroppedOversizeIn: c.droppedOversizeIn.Load(), DroppedOversizeOut: c.droppedOversizeOut.Load(),
			UpstreamRefused: c.upstreamRefused.Load(), StreamsReset: c.streamsReset.Load(), Panics: c.panics.Load(),
		}
	}
	return st
}
