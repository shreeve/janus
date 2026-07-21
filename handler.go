package janus

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is the site-level data-plane admission module.
type Handler struct {
	// Ping overrides the global ping default for this site when non-nil.
	Ping *bool `json:"ping,omitempty"`

	// Cache overrides the global cache default/tuning for this site when
	// non-nil (process-wide keys are illegal here).
	Cache *CacheSettings `json:"cache,omitempty"`

	// Hub overrides the global hub default/tuning for this site when
	// non-nil.
	Hub *HubSettings `json:"hub,omitempty"`

	app    *App
	dp     *dataPlane
	logger *zap.Logger

	// cacheCfg is the site's effective cache configuration; nil when the
	// effective cache is off, so the bypass path costs one nil check.
	cacheCfg *cacheSite

	// hubCfg is the site's effective hub configuration; nil when the
	// effective hub is off, so the bypass path costs one nil check.
	hubCfg *hubSite
}

// CaddyModule returns the Caddy module information.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.janus",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the handler.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	appI, err := ctx.AppIfConfigured("janus")
	if err != nil && !errors.Is(err, caddy.ErrNotConfigured) {
		return err
	}
	if appI != nil {
		app, ok := appI.(*App)
		if !ok {
			return fmt.Errorf("janus handler: app is unexpected type %T", appI)
		}
		h.app = app
		h.dp = app.dp
	}
	if err := h.provisionCache(); err != nil {
		return err
	}
	if err := h.provisionHub(); err != nil {
		return err
	}
	h.logger.Info("janus handler ready",
		zap.Bool("ping", h.pingEnabled()),
		zap.Bool("cache", h.cacheCfg != nil),
		zap.Bool("hub", h.hubCfg != nil),
	)
	return nil
}

func (h *Handler) pingEnabled() bool {
	var global *bool
	if h.app != nil {
		global = h.app.Ping
	}
	return cascadeBool(h.Ping, global, false)
}

// ServeHTTP handles admitted requests: site-scoped /ping answers first when
// enabled; everything else routes through the data plane (registry hosts →
// upstreams; unknown hosts → 404).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if h.pingEnabled() && (r.URL.Path == "/ping" || r.URL.Path == "/ping/") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("pong\n"))
		return err
	}
	// Hub interception (before cache and upstream selection, after ping):
	// the hub claims upgrades to its path only — a non-upgrade request to
	// the same path flows through the data plane like any other.
	if h.hubCfg != nil && r.URL.Path == h.hubCfg.path && websocket.IsWebSocketUpgrade(r) {
		return h.serveHub(w, r)
	}
	if h.dp != nil {
		if h.cacheCfg != nil {
			return h.serveCache(w, r)
		}
		return h.dp.serve(w, r)
	}
	return caddyhttp.Error(http.StatusNotFound, nil)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//	janus
//	janus {
//	    ping
//	    ping off
//	    cache off
//	    cache { ttl 5s; debug }
//	}
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume "janus"
	if d.NextArg() {
		return d.ArgErr()
	}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		switch d.Val() {
		case "ping":
			on, err := parseOnOff(d.RemainingArgs())
			if err != nil {
				return d.Errf("ping: %v", err)
			}
			h.Ping = &on
		case "cache":
			if h.Cache != nil {
				return d.Err("duplicate cache directive in the same block")
			}
			cs, err := parseCacheDirective(d, false)
			if err != nil {
				return err
			}
			h.Cache = cs
		case "hub":
			if h.Hub != nil {
				return d.Err("duplicate hub directive in the same block")
			}
			hs, err := parseHubDirective(d)
			if err != nil {
				return err
			}
			h.Hub = hs
		case "control":
			return d.Err("control is process-wide; configure it in the global janus options block")
		default:
			return d.Errf("unrecognized janus subdirective: %s", d.Val())
		}
	}
	return nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
