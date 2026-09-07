package janus

import (
	"context"
	"errors"
	"fmt"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
)

func init() {
	caddy.RegisterModule(PermissionByJanus{})
}

// PermissionByJanus is Caddy's on-demand TLS permission, answered by the
// janus app in-process: a certificate may be minted for a host of a
// registered app or a cold browse claim, and for nothing else — the rule
// GET /1.0/tls/ask applies, with no listener for Caddy to dial and no
// round trip on a first handshake.
//
//	on_demand_tls {
//		permission janus
//	}
type PermissionByJanus struct {
	// app resolves the janus app at ask time. The tls app provisions
	// this module, and the janus app is not necessarily loaded then;
	// looking it up at provision would order the apps by accident.
	app func() (*App, error)
}

// CaddyModule returns the Caddy module information.
func (PermissionByJanus) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "tls.permission.janus",
		New: func() caddy.Module { return new(PermissionByJanus) },
	}
}

// UnmarshalCaddyfile parses `permission janus`; it takes no arguments.
func (p *PermissionByJanus) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next()
	if d.NextArg() {
		return d.ArgErr()
	}
	return nil
}

// Provision binds the lookup to this config's janus app.
func (p *PermissionByJanus) Provision(ctx caddy.Context) error {
	p.app = func() (*App, error) {
		appI, err := ctx.AppIfConfigured("janus")
		if err != nil && !errors.Is(err, caddy.ErrNotConfigured) {
			return nil, err
		}
		app, _ := appI.(*App)
		if app == nil {
			return nil, errors.New("janus tls permission: this config has no janus app, so no registry approves on-demand names")
		}
		return app, nil
	}
	return nil
}

// CertificateAllowed is the on-demand decision for one name. A nil
// return mints; any error denies.
func (p *PermissionByJanus) CertificateAllowed(_ context.Context, name string) error {
	app, err := p.app()
	if err != nil {
		return err
	}
	if _, err := app.certificateAllowed(name); err != nil {
		return fmt.Errorf("janus tls permission: %w", err)
	}
	return nil
}

var (
	_ caddytls.OnDemandPermission = (*PermissionByJanus)(nil)
	_ caddyfile.Unmarshaler       = (*PermissionByJanus)(nil)
	_ caddy.Provisioner           = (*PermissionByJanus)(nil)
)
