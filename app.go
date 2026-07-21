package janus

import (
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the process-wide Janus application (cold config).
type App struct {
	// Control is the exact set of control-plane listeners.
	// Empty means default: one implicit "control internal".
	Control []Control `json:"control,omitempty"`

	// Ping is the global default for the site-scoped ping capability.
	// nil = built-in default (off). Sites may override.
	Ping *bool `json:"ping,omitempty"`

	// Cache is the global default for the site-scoped cache capability
	// (micro-cache + coalescing), plus the process-wide pool knobs
	// (max_bytes, max_app_share). nil = built-in default (off).
	Cache *CacheSettings `json:"cache,omitempty"`

	// Hub is the global default for the site-scoped hub capability
	// (per-app WebSocket fan-out). nil = built-in default (off).
	Hub *HubSettings `json:"hub,omitempty"`

	logger      *zap.Logger
	ctx         caddy.Context
	controlSrvs []*controlServer

	// state is the pooled process state (caddy.UsagePool): registry, data
	// plane, and hubs survive Caddy config reloads; only process shutdown
	// releases them.
	state *janusState

	// appsReg is the memory-only apps registry (hot /1.0/apps).
	appsReg *appRegistry

	// dp routes admitted data-plane requests (host → upstream, ring).
	dp *dataPlane

	// hubs is the per-app hub set (Phase 7).
	hubs *hubSet

	// hubSites pairs compiled site routes with effective hub configs, per
	// server; built at Start for max_conns floor resolution.
	hubSites [][]hubSiteEntry

	// cache is the one process-wide micro-cache pool. Always constructed
	// (the /1.0/cache counters are always on); sites opt in via cascade.
	cache *cacheStore
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "janus",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision sets up the app. Registry, data plane, and hub state come
// from the pooled process holder, so a config reload binds the new app to
// the same live state instead of constructing a split-brain registry.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	a.ctx = ctx
	stI, _, err := janusPool.LoadOrNew(janusStateKey, func() (caddy.Destructor, error) {
		return newJanusState(a.logger)
	})
	if err != nil {
		return err
	}
	a.state = stI.(*janusState)
	a.appsReg = a.state.registry
	a.dp = a.state.dp
	a.hubs = a.state.hubs
	if err := a.provisionCacheStore(); err != nil {
		return err
	}
	if len(a.Control) == 0 {
		a.Control = []Control{{Mode: "internal"}}
	}
	seen := map[string]bool{}
	for i := range a.Control {
		if err := a.Control[i].normalize(); err != nil {
			return fmt.Errorf("janus: %w", err)
		}
		if seen[a.Control[i].Mode] {
			return fmt.Errorf("janus: duplicate control mode %q", a.Control[i].Mode)
		}
		seen[a.Control[i].Mode] = true
	}
	return nil
}

// Start starts the Janus app.
func (a *App) Start() error {
	a.logger.Info("janus ping default",
		zap.Bool("enabled", resolveBool(nil, a.Ping, false)),
	)
	// The HTTP app is provisioned by now: pair each janus site with its
	// host matchers (max_conns floor resolution), then close hub
	// connections whose host's effective hub flipped off in this config.
	if err := a.buildHubSiteTable(); err != nil {
		return err
	}
	a.closeDisabledHubHosts()
	if err := a.startControlListeners(); err != nil {
		// A partially started app never receives Stop: close whatever
		// listeners came up before rejecting.
		if serr := a.stopControlListeners(); serr != nil {
			a.logger.Error("janus control unwind", zap.Error(serr))
		}
		return err
	}
	return nil
}

// Stop stops the Janus app. Pooled state (registry sweeper, hubs, open
// sockets) deliberately survives: a config reload stops the old app while
// the new one is already serving the same pooled state.
func (a *App) Stop() error {
	return a.stopControlListeners()
}

// Cleanup releases the app's reference on the pooled state; the last
// release (process shutdown) destructs it.
func (a *App) Cleanup() error {
	if a.state != nil {
		_, err := janusPool.Delete(janusStateKey)
		return err
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module       = (*App)(nil)
	_ caddy.App          = (*App)(nil)
	_ caddy.Provisioner  = (*App)(nil)
	_ caddy.CleanerUpper = (*App)(nil)
)
