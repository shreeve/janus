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

	logger      *zap.Logger
	controlSrvs []*controlServer

	// appsReg is the memory-only apps registry (hot /1.0/apps).
	appsReg *appRegistry
}

// CaddyModule returns the Caddy module information.
func (App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "janus",
		New: func() caddy.Module { return new(App) },
	}
}

// Provision sets up the app.
func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	a.appsReg = newAppRegistry()
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
	return a.startControlListeners()
}

// Stop stops the Janus app.
func (a *App) Stop() error {
	return a.stopControlListeners()
}

// Interface guards
var (
	_ caddy.Module      = (*App)(nil)
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
