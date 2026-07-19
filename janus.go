// Package janus is a Caddy HTTP handler module.
//
// This build is ping-only: it proves module registration and HTTPS
// admission. GET /ping returns pong; every other path is 404.
package janus

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(Janus{})
	httpcaddyfile.RegisterHandlerDirective("janus", parseCaddyfile)
	httpcaddyfile.RegisterDirectiveOrder("janus", httpcaddyfile.Before, "respond")
}

// Janus is the ping-only edge handler.
type Janus struct {
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (Janus) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.janus",
		New: func() caddy.Module { return new(Janus) },
	}
}

// Provision sets up the module.
func (j *Janus) Provision(ctx caddy.Context) error {
	j.logger = ctx.Logger()
	j.logger.Info("janus ready (ping-only)")
	return nil
}

// ServeHTTP handles admitted requests.
func (j *Janus) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	if r.URL.Path == "/ping" || r.URL.Path == "/ping/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("pong\n"))
		return err
	}
	return caddyhttp.Error(http.StatusNotFound, nil)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//	janus
func (j *Janus) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume "janus"
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var j Janus
	err := j.UnmarshalCaddyfile(h.Dispenser)
	return &j, err
}

// Interface guards
var (
	_ caddy.Provisioner             = (*Janus)(nil)
	_ caddyhttp.MiddlewareHandler   = (*Janus)(nil)
	_ caddyfile.Unmarshaler         = (*Janus)(nil)
)
