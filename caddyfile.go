package janus

import (
	"encoding/json"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterGlobalOption("janus", parseGlobalJanus)
	httpcaddyfile.RegisterHandlerDirective("janus", parseHandlerJanus)
	httpcaddyfile.RegisterDirectiveOrder("janus", httpcaddyfile.Before, "respond")
}

// parseGlobalJanus configures the process-wide Janus app from the global options block.
//
//	{
//	    janus {
//	        control local
//	        ping
//	    }
//	}
func parseGlobalJanus(d *caddyfile.Dispenser, existing any) (any, error) {
	app := new(App)
	if existing != nil {
		if prev, ok := existing.(httpcaddyfile.App); ok {
			if err := json.Unmarshal(prev.Value, app); err != nil {
				return nil, d.Errf("decoding existing janus app: %v", err)
			}
		}
	}

	if err := app.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}

	return httpcaddyfile.App{
		Name:  "janus",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

// UnmarshalCaddyfile parses the global janus block.
func (a *App) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			switch d.Val() {
			case "control":
				c, err := parseControl(d)
				if err != nil {
					return err
				}
				a.Control = append(a.Control, c)
			case "ping":
				on, err := parseOnOff(d.RemainingArgs())
				if err != nil {
					return d.Errf("ping: %v", err)
				}
				a.Ping = &on
			default:
				return d.Errf("unrecognized janus subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

// parseControl parses:
//
//	control internal
//	control internal <path>
//	control local
//	control local <host:port>
//	control public <host:port>
func parseControl(d *caddyfile.Dispenser) (Control, error) {
	var c Control
	if !d.NextArg() {
		return c, d.ArgErr()
	}
	c.Mode = d.Val()
	switch c.Mode {
	case "internal", "local":
		if d.NextArg() {
			c.Listen = d.Val()
		}
		if d.NextArg() {
			return c, d.ArgErr()
		}
	case "public":
		if !d.NextArg() {
			return c, d.Err("control public requires exactly one host:port")
		}
		c.Listen = d.Val()
		if d.NextArg() {
			return c, d.ArgErr()
		}
	default:
		return c, d.Errf("unknown control mode %q (want internal, local, or public)", c.Mode)
	}
	if d.NextBlock(d.Nesting()) {
		return c, d.Err("control does not support a nested block")
	}
	return c, nil
}

func parseHandlerJanus(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var j Handler
	err := j.UnmarshalCaddyfile(h.Dispenser)
	return &j, err
}
