package janus

import (
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

// Pooled process state (docs/20260720-162350-hub-design.md "Caddy config
// reload"). App.Provision acquires this holder from caddy.UsagePool (or
// constructs it once), so newly provisioned handlers bind to the same
// registry and hub entries before the old config retires — no split-brain
// registry, and a live hub socket keeps its connection id, memberships,
// and fan-out ability across a successful Caddy reload. The holder owns
// the registry sweeper; releasing an old module reference neither stops
// the sweeper nor tears down a hub. Final process cleanup releases the
// holder. Only registry DELETE or TTL reap deletes a pooled app entry.
var janusPool = caddy.NewUsagePool()

const janusStateKey = "janus.process"

type janusState struct {
	registry *appRegistry
	hubs     *hubSet
	dp       *dataPlane
	logger   *zap.Logger
}

func newJanusState(logger *zap.Logger) (*janusState, error) {
	ttl, err := heartbeatTTLFromEnv()
	if err != nil {
		return nil, fmt.Errorf("janus: %w", err)
	}
	reg := newAppRegistry()
	reg.ttl = ttl
	st := &janusState{
		registry: reg,
		hubs:     newHubSet(),
		dp:       newDataPlane(reg, logger),
		logger:   logger,
	}
	// DELETE and TTL reap tear the app's hub down; PATCH host removal
	// closes the removed hosts' connections. Wired once — the hub set and
	// the registry live and die together in this holder.
	reg.hubTeardown = st.hubs.teardownApp
	reg.hubHostsRemoved = st.hubs.hostsRemoved
	reg.startSweeper(logger)
	return st, nil
}

// Destruct runs when the last config generation using the holder is
// released: the process is done with Janus entirely.
func (st *janusState) Destruct() error {
	st.registry.stopSweeper()
	for _, h := range st.hubs.snapshotAll() {
		st.hubs.teardownApp(h.id)
	}
	return nil
}

var _ caddy.Destructor = (*janusState)(nil)
