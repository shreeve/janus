package janus

import (
	"encoding/json"
	"strings"

	"github.com/caddyserver/caddy/v2"
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
			case "cache":
				if a.Cache != nil {
					return d.Err("duplicate cache directive in the same block")
				}
				cs, err := parseCacheDirective(d, true)
				if err != nil {
					return err
				}
				a.Cache = cs
			case "hub":
				if a.Hub != nil {
					return d.Err("duplicate hub directive in the same block")
				}
				hs, err := parseHubDirective(d)
				if err != nil {
					return err
				}
				a.Hub = hs
			case "mdns":
				if a.Mdns != nil {
					return d.Err("duplicate mdns directive in the same block")
				}
				ms, err := parseMdnsDirective(d)
				if err != nil {
					return err
				}
				a.Mdns = ms
			case "auth":
				if a.Auth != nil {
					return d.Err("duplicate auth directive in the same block")
				}
				as, err := parseAuthDirective(d)
				if err != nil {
					return err
				}
				a.Auth = as
			case "heartbeat_ttl":
				if a.HeartbeatTTL != 0 {
					return d.Err("duplicate heartbeat_ttl directive")
				}
				val, err := oneDirectiveArg(d, "janus", "heartbeat_ttl")
				if err != nil {
					return err
				}
				dur, perr := caddy.ParseDuration(val)
				if perr != nil || dur <= 0 {
					return d.Errf("heartbeat_ttl: want a positive duration (e.g. 15s), got %q", val)
				}
				a.HeartbeatTTL = caddy.Duration(dur)
			case "token":
				return d.Err("token belongs on the control line as token:… (per-listener), not as its own directive")
			default:
				return d.Errf("unrecognized janus subdirective: %s", d.Val())
			}
		}
	}
	return nil
}

// parseControl parses a self-contained control line:
//
//	control internal
//	control internal <socket-path>
//	control local
//	control local http://127.0.0.1:7600/
//	control local http://127.0.0.1:7600/ token:ENV
//	control public
//	control public https://0.0.0.0:7601/ token:JANUS_TOKEN
//	control public token:./secrets/janus.auth
//	control public token:JANUS_TOKEN cert:/etc/janus/tls.crt key:/etc/janus/tls.key
func parseControl(d *caddyfile.Dispenser) (Control, error) {
	var c Control
	if !d.NextArg() {
		return c, d.ArgErr()
	}
	c.Mode = d.Val()
	switch c.Mode {
	case "internal", "local", "public":
	default:
		return c, d.Errf("unknown control mode %q (want internal, local, or public)", c.Mode)
	}

	for d.NextArg() {
		tok := d.Token()
		val := tok.Text
		quoted := tok.Quoted()
		if strings.HasPrefix(val, "token:") {
			kind, ref, err := parseTokenArg(val, quoted)
			if err != nil {
				return c, d.Err(err.Error())
			}
			if c.TokenKind != "" {
				return c, d.Err("control line has more than one token:…")
			}
			c.TokenKind = kind
			c.Token = ref
			continue
		}
		if strings.HasPrefix(val, "cert:") {
			path := strings.TrimPrefix(val, "cert:")
			if path == "" {
				return c, d.Err("cert: value is empty")
			}
			if c.CertFile != "" {
				return c, d.Err("control line has more than one cert:…")
			}
			c.CertFile = path
			continue
		}
		if strings.HasPrefix(val, "key:") {
			path := strings.TrimPrefix(val, "key:")
			if path == "" {
				return c, d.Err("key: value is empty")
			}
			if c.KeyFile != "" {
				return c, d.Err("control line has more than one key:…")
			}
			c.KeyFile = path
			continue
		}
		if c.Listen != "" {
			return c, d.Errf("unexpected control argument %q", val)
		}
		c.Listen = val
	}
	if d.NextBlock(d.Nesting()) {
		return c, d.Err("control does not support a nested block; keep it on one line")
	}
	return c, nil
}

func parseHandlerJanus(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var j Handler
	err := j.UnmarshalCaddyfile(h.Dispenser)
	return &j, err
}
