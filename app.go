package janus

import (
	"fmt"
	"net/http"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
}

// App is the process-wide Janus application (cold config).
type App struct {
	// Control is the exact set of control-plane listeners serving the
	// hot /1.0 API. Empty means one implicit internal (unix socket)
	// listener.
	Control []Control `json:"control,omitempty"`

	// Ping is the global default for the site-scoped ping capability.
	// Default: off. Sites may override.
	Ping *bool `json:"ping,omitempty"`

	// Cache is the global default and process-wide pool configuration
	// for the site-scoped micro-cache. Default: off. Sites may override
	// the per-site keys.
	Cache *CacheSettings `json:"cache,omitempty"`

	// Hub is the global default for the site-scoped hub capability
	// (per-app WebSocket fan-out). Default: off. Sites may override.
	Hub *HubSettings `json:"hub,omitempty"`

	// Mdns is the process-wide LAN-presence capability: the advertised
	// .local identity, per-app .local advertising, and the plain-HTTP
	// front door. Default: off (nil).
	Mdns *MdnsSettings `json:"mdns,omitempty"`

	// Auth is the global default for the site-scoped auth wall (edge
	// authentication for auth-less apps): default posture plus the
	// default user set. Default: off. Sites may override.
	Auth *AuthSettings `json:"auth,omitempty"`

	// HeartbeatTTL is how long a registered app may go without a
	// heartbeat before its registration is reaped (same effect as
	// DELETE). Default: 15s. The JANUS_HEARTBEAT_TTL environment
	// variable is honored as a fallback when this is unset.
	HeartbeatTTL caddy.Duration `json:"heartbeat_ttl,omitempty"`

	logger      *zap.Logger
	hubLog      *zap.Logger // named child logger for the hub subsystem
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

	// mdnsSrv is this config generation's front-door HTTP server
	// (dedicated mode only); the pooled advertiser itself lives in
	// state.mdns.
	mdnsSrv *http.Server

	// mdnsSharedPort and mdnsSharedRoutes wire the shared-mode front
	// door (mdns with no listen): janus site handlers on the HTTP app's
	// plain-HTTP port serve mdnsSharedRoutes for front-door Hosts and
	// pass everything else through. Set at Start by startMdns; zero in
	// dedicated mode and with mdns off.
	mdnsSharedPort   int
	mdnsSharedRoutes http.Handler

	// authSites pairs compiled site routes with effective auth configs;
	// built at Start for the removed-user session revocation, the
	// reaper's ttl bound, and the /1.0/auth sites view.
	authSites []authSiteEntry
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
	a.hubLog = a.logger.Named("hub")
	a.ctx = ctx
	if a.HeartbeatTTL < 0 {
		return fmt.Errorf("janus: heartbeat_ttl must be positive, got %v", time.Duration(a.HeartbeatTTL))
	}
	stI, _, err := janusPool.LoadOrNew(janusStateKey, func() (caddy.Destructor, error) {
		return newJanusState(a.logger, time.Duration(a.HeartbeatTTL))
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
	if err := a.provisionMdns(); err != nil {
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
		zap.Bool("enabled", cascadeBool(nil, a.Ping, false)),
	)
	// The HTTP app is provisioned by now: pair each janus site with its
	// host matchers (max_conns floor resolution), then close hub
	// connections whose host's effective hub flipped off in this config.
	if err := a.buildHubSiteTable(); err != nil {
		return err
	}
	a.closeDisabledHubHosts()
	// Auth Start work runs past the reload's point of no return (the
	// mdns introspection precedent): removed-user session revocation
	// and the reaper's ttl bound — an aborted reload never logs users
	// out.
	if err := a.startAuth(); err != nil {
		return err
	}
	if err := a.startControlListeners(); err != nil {
		// A partially started app never receives Stop: close whatever
		// listeners came up before rejecting.
		if serr := a.stopControlListeners(); serr != nil {
			a.logger.Error("janus control unwind", zap.Error(serr))
		}
		return err
	}
	if err := a.startMdns(); err != nil {
		if serr := a.stopControlListeners(); serr != nil {
			a.logger.Error("janus control unwind", zap.Error(serr))
		}
		if serr := a.stopMdns(); serr != nil {
			a.logger.Error("janus mdns unwind", zap.Error(serr))
		}
		return err
	}
	return nil
}

// Stop stops the Janus app. Pooled state (registry sweeper, hubs, open
// sockets, the mdns advertiser) deliberately survives: a config reload
// stops the old app while the new one is already serving the same pooled
// state.
func (a *App) Stop() error {
	cerr := a.stopControlListeners()
	merr := a.stopMdns()
	if cerr != nil {
		return cerr
	}
	return merr
}

// Cleanup releases the app's reference on the pooled state; the last
// release (process shutdown) destructs it. A release that leaves other
// generations holding the state is either a successful reload's old
// generation retiring or an aborted reload's new generation being torn
// down — the advertiser tells them apart and ERROR-logs the aborted
// case's config divergence.
func (a *App) Cleanup() error {
	if a.state != nil {
		deleted, err := janusPool.Delete(janusStateKey)
		if !deleted {
			a.state.mdns.generationRetired(a)
		}
		return err
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module          = (*App)(nil)
	_ caddy.App             = (*App)(nil)
	_ caddy.Provisioner     = (*App)(nil)
	_ caddy.CleanerUpper    = (*App)(nil)
	_ caddyfile.Unmarshaler = (*App)(nil)
)
