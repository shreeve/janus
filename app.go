package janus

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(new(App))
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

	// Files is the global default for the site-scoped registered-file
	// service. Default: off. Sites may override.
	Files *bool `json:"files,omitempty"`

	// FilesPrecompressed is the process-wide ordered set of sidecar
	// representations admitted by the global files capability. Site
	// blocks may turn files on or off but cannot retune this order.
	FilesPrecompressed []string `json:"files_precompressed,omitempty"`

	// Browse is the process-wide theme and renderer configuration.
	// Presence also enables the global site-scoped browse default.
	Browse *BrowseSettings `json:"browse,omitempty"`

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

	// access is the separately pooled access bridge. Stream ownership is
	// generation-local even though registration sequence state is pooled.
	access             *accessBridge
	accessReleased     bool
	accessStreamsMu    sync.Mutex
	accessStreams      map[*accessSubscriber]struct{}
	accessStreamsWG    sync.WaitGroup
	accessStopping     bool
	accessStopDeadline time.Time

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
	// purgeOwner identifies this generation's cache hook without exposing the
	// registry to arbitrary, potentially non-comparable map keys.
	purgeOwner *purgeOwner

	// mdnsSrv is this config generation's front-door HTTP server
	// (dedicated mode only); the pooled advertiser itself lives in
	// state.mdns.
	mdnsSrv      *http.Server
	mdnsLn       net.Listener
	mdnsStopping atomic.Bool

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

	browseSites   []browseSiteEntry
	browseRuntime *BrowseSettings
	browseCtx     context.Context
	browseCancel  context.CancelFunc
	coldHosts     []string

	reconcileMu      sync.Mutex
	reconcilePending bool
}

// CaddyModule returns the Caddy module information.
func (*App) CaddyModule() caddy.ModuleInfo {
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
	if a.HeartbeatTTL != 0 && time.Duration(a.HeartbeatTTL) < minHeartbeatTTL {
		return fmt.Errorf("janus: heartbeat_ttl must be at least %v, got %v", minHeartbeatTTL, time.Duration(a.HeartbeatTTL))
	}
	access, err := acquireAccessBridge(a.logger)
	if err != nil {
		return err
	}
	a.access = access
	a.accessStreams = make(map[*accessSubscriber]struct{})
	stI, _, err := janusPool.LoadOrNew(janusStateKey, func() (caddy.Destructor, error) {
		return newJanusState(a.logger, time.Duration(a.HeartbeatTTL))
	})
	if err != nil {
		return err
	}
	a.state = stI.(*janusState)
	if err := a.state.registry.bindAccess(a.access); err != nil {
		return err
	}
	a.appsReg = a.state.registry
	a.dp = a.state.dp
	a.hubs = a.state.hubs
	a.browseCtx, a.browseCancel = context.WithCancel(ctx)
	if a.Browse != nil && a.Files != nil && !*a.Files {
		return fmt.Errorf("janus browse: global browse conflicts with global files off")
	}
	if err := validateFilesPrecompressed(a.Files, a.FilesPrecompressed); err != nil {
		return err
	}
	if err := a.provisionBrowse(); err != nil {
		return err
	}
	if err := a.provisionCacheStore(); err != nil {
		return err
	}
	if err := a.provisionMdns(); err != nil {
		return err
	}
	if len(a.Control) == 0 {
		a.Control = []Control{{Mode: "internal"}}
	}
	return normalizeControls(a.Control)
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
	if err := a.prepareAuth(); err != nil {
		return err
	}
	if err := a.startBrowse(); err != nil {
		return err
	}
	if err := a.startControlListeners(); err != nil {
		// A partially started app never receives Stop: close whatever
		// listeners came up before rejecting.
		a.stopAccessStreams()
		if serr := a.stopControlListeners(); serr != nil {
			a.logger.Error("janus control unwind", zap.Error(serr))
		}
		a.stopBrowse()
		return err
	}
	if err := a.startMdns(); err != nil {
		a.stopAccessStreams()
		if serr := a.stopControlListeners(); serr != nil {
			a.logger.Error("janus control unwind", zap.Error(serr))
		}
		if serr := a.stopMdns(); serr != nil {
			a.logger.Error("janus mdns unwind", zap.Error(serr))
		}
		a.stopBrowse()
		return err
	}
	a.stageReconciliation()
	if !a.hasActivePredecessor() {
		a.commitReconciliation()
	}
	return nil
}

// Stop stops the Janus app. Pooled state (registry sweeper, hubs, open
// sockets, the mdns advertiser) deliberately survives: a config reload
// stops the old app while the new one is already serving the same pooled
// state.
func (a *App) Stop() error {
	// Caddy swaps ActiveContext only after every new app has started. The old
	// generation's Stop is therefore the first safe commit signal for pooled,
	// irreversible reconciliation. An aborted new generation's Stop still sees
	// the old active app, whose reconciliation is already complete.
	a.commitActiveSuccessor()
	return a.stopGenerationResources()
}

// stopGenerationResources tears down everything owned by this config
// generation. It is deliberately idempotent: Caddy normally calls Stop before
// Cleanup, but may call Cleanup directly when a fully started candidate fails
// later in config setup.
func (a *App) stopGenerationResources() error {
	a.stopBrowse()
	a.stopAccessStreams()
	return errors.Join(a.stopControlListeners(), a.stopMdns())
}

// Cleanup first tears down this generation's resources, including when Caddy
// rejects a started candidate without calling Stop. It then releases the app's
// reference on the pooled state; the last release (process shutdown) destructs
// it. A release that leaves other generations holding the state is either a
// successful reload's old generation retiring or an aborted reload's new
// generation being torn down — the advertiser tells them apart and rolls an
// aborted generation back to its surviving predecessor.
func (a *App) Cleanup() error {
	generationErr := a.stopGenerationResources()
	if a.state != nil {
		a.state.browse.releaseCold(a)
	}
	var stateErr error
	if a.state != nil {
		if a.purgeOwner != nil {
			a.state.registry.unbindPurge(a.purgeOwner)
		}
		deleted, err := janusPool.Delete(janusStateKey)
		if !deleted {
			a.state.mdns.generationRetired(a)
		}
		stateErr = err
		a.state = nil
	}
	if a.access != nil && !a.accessReleased {
		a.accessReleased = true
		accessErr := releaseAccessBridge()
		a.access = nil
		stateErr = errors.Join(stateErr, accessErr)
	}
	return errors.Join(generationErr, stateErr)
}

func (a *App) stageReconciliation() {
	a.reconcileMu.Lock()
	a.reconcilePending = true
	a.reconcileMu.Unlock()
}

func (a *App) commitReconciliation() bool {
	a.reconcileMu.Lock()
	if !a.reconcilePending {
		a.reconcileMu.Unlock()
		return false
	}
	a.reconcilePending = false
	a.reconcileMu.Unlock()
	a.closeDisabledHubHosts()
	a.reconcileAuth()
	return true
}

func (a *App) activePredecessor() *App {
	active, err := caddy.ActiveContext().AppIfConfigured("janus")
	if err != nil || active == nil {
		return nil
	}
	app, ok := active.(*App)
	if !ok || app == a || app.state == nil || app.state != a.state {
		return nil
	}
	return app
}

func (a *App) hasActivePredecessor() bool { return a.activePredecessor() != nil }

func (a *App) commitSuccessor(next *App) bool {
	if next == nil || next == a || next.state == nil || next.state != a.state {
		return false
	}
	return next.commitReconciliation()
}

func (a *App) commitActiveSuccessor() {
	active, err := caddy.ActiveContext().AppIfConfigured("janus")
	if err != nil || active == nil {
		return
	}
	next, ok := active.(*App)
	if ok {
		a.commitSuccessor(next)
	}
}

func (a *App) stopAccessStreams() {
	deadline := time.Now().Add(accessWriteDeadline)
	a.accessStreamsMu.Lock()
	if a.accessStopping {
		a.accessStreamsMu.Unlock()
		return
	}
	a.accessStopping = true
	a.accessStopDeadline = deadline
	subs := make([]*accessSubscriber, 0, len(a.accessStreams))
	for sub := range a.accessStreams {
		subs = append(subs, sub)
	}
	a.accessStreamsMu.Unlock()
	for _, sub := range subs {
		if sub.controller != nil {
			_ = sub.controller.SetWriteDeadline(deadline)
		}
		a.detachAccessSubscriber(sub, "generation_stop")
	}
	done := make(chan struct{})
	go func() {
		a.accessStreamsWG.Wait()
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		for _, server := range a.controlSrvs {
			server.closeConnections()
		}
		<-done
	}
}

// Interface guards
var (
	_ caddy.Module          = (*App)(nil)
	_ caddy.App             = (*App)(nil)
	_ caddy.Provisioner     = (*App)(nil)
	_ caddy.CleanerUpper    = (*App)(nil)
	_ caddyfile.Unmarshaler = (*App)(nil)
)
