package janus

import (
	"fmt"
	"net"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(App{})
}

// DefaultControlLocal is the loopback bind used by `control local`.
const DefaultControlLocal = "127.0.0.1:7600"

// DefaultControlInternal is the unix socket path used by `control internal`.
const DefaultControlInternal = "run/janus.sock"

// App is the process-wide Janus application (cold config).
type App struct {
	// Control is one or more control-plane listeners (process-wide; does not cascade).
	Control []Control `json:"control,omitempty"`

	// Ping is the global default for the site-scoped ping capability.
	// nil = built-in default (off). Sites may override.
	Ping *bool `json:"ping,omitempty"`

	logger *zap.Logger
}

// Control is a single control-plane reachability mode.
type Control struct {
	// Mode is internal, local, or public.
	Mode string `json:"mode,omitempty"`

	// Listen is a unix path (internal) or host:port (local/public).
	// Empty means the mode default (internal/local only).
	Listen string `json:"listen,omitempty"`
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
	if len(a.Control) == 0 {
		return fmt.Errorf("janus: at least one control listener is required (internal, local, or public)")
	}
	seen := map[string]bool{}
	for i := range a.Control {
		if err := a.Control[i].normalize(); err != nil {
			return err
		}
		if seen[a.Control[i].Mode] {
			return fmt.Errorf("janus: duplicate control mode %q", a.Control[i].Mode)
		}
		seen[a.Control[i].Mode] = true
	}
	return nil
}

// Start starts the Janus app.
//
// Control listeners are validated and logged here. Serving /1.0 is Phase 2.
func (a *App) Start() error {
	for _, c := range a.Control {
		a.logger.Info("janus control configured",
			zap.String("mode", c.Mode),
			zap.String("listen", c.Listen),
		)
	}
	a.logger.Info("janus ping default",
		zap.Bool("enabled", resolveBool(nil, a.Ping, false)),
	)
	return nil
}

// Stop stops the Janus app.
func (a *App) Stop() error {
	return nil
}

func (c *Control) normalize() error {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	switch mode {
	case "internal":
		c.Mode = "internal"
		if c.Listen == "" {
			c.Listen = DefaultControlInternal
		}
		return nil
	case "local":
		c.Mode = "local"
		if c.Listen == "" {
			c.Listen = DefaultControlLocal
		}
		if err := requireHostPort(c.Listen); err != nil {
			return fmt.Errorf("janus: control local: %w", err)
		}
		host, _, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return fmt.Errorf("janus: control local: %w", err)
		}
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return fmt.Errorf("janus: control local must bind loopback (127.0.0.1, ::1, or localhost), got %q", host)
		}
		return nil
	case "public":
		c.Mode = "public"
		if c.Listen == "" {
			return fmt.Errorf("janus: control public requires an address (host:port)")
		}
		if err := requireHostPort(c.Listen); err != nil {
			return fmt.Errorf("janus: control public: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("janus: unknown control mode %q (want internal, local, or public)", c.Mode)
	}
}

func requireHostPort(addr string) error {
	_, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address must be host:port, got %q", addr)
	}
	return nil
}

// Interface guards
var (
	_ caddy.Module      = (*App)(nil)
	_ caddy.App         = (*App)(nil)
	_ caddy.Provisioner = (*App)(nil)
)
