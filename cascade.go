package janus

import (
	"errors"
	"fmt"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// Cascade resolution, shared by every site-scoped capability: site
// override → global default → built-in default. One function per value
// shape; one rule everywhere.

func cascadeBool(site, global *bool, builtin bool) bool {
	if site != nil {
		return *site
	}
	if global != nil {
		return *global
	}
	return builtin
}

func cascadeDuration(site, global *caddy.Duration, builtin time.Duration) time.Duration {
	if site != nil {
		return time.Duration(*site)
	}
	if global != nil {
		return time.Duration(*global)
	}
	return builtin
}

func cascadeInt64(site, global *int64, builtin int64) int64 {
	if site != nil {
		return *site
	}
	if global != nil {
		return *global
	}
	return builtin
}

// parseOnOff parses trailing args for a boolean capability directive.
//
//	ping           → true
//	ping on        → true
//	ping off       → false
func parseOnOff(args []string) (bool, error) {
	switch len(args) {
	case 0:
		return true, nil
	case 1:
		switch args[0] {
		case "on":
			return true, nil
		case "off":
			return false, nil
		default:
			return false, fmt.Errorf(`want "on" or "off", got %q`, args[0])
		}
	default:
		return false, errors.New(`want at most one of "on" or "off"`)
	}
}

// oneDirectiveArg consumes exactly one argument for a subdirective,
// erroring with the directive-qualified position.
func oneDirectiveArg(d *caddyfile.Dispenser, directive, sub string) (string, error) {
	args := d.RemainingArgs()
	if len(args) != 1 {
		return "", d.Errf("%s %s: want exactly one argument", directive, sub)
	}
	return args[0], nil
}
