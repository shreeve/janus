package janus

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Handler{})
}

// Handler is the site-level data-plane admission module.
type Handler struct {
	// Ping overrides the global ping default for this site when non-nil.
	Ping *bool `json:"ping,omitempty"`

	app    *App
	dp     *dataPlane
	logger *zap.Logger
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
	h.logger.Info("janus handler ready",
		zap.Bool("ping", h.pingEnabled()),
	)
	return nil
}

func (h *Handler) pingEnabled() bool {
	var global *bool
	if h.app != nil {
		global = h.app.Ping
	}
	return resolveBool(h.Ping, global, false)
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
	if h.dp != nil {
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
