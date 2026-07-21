package janus

import (
	"fmt"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/dustin/go-humanize"
)

// CacheSettings configures the site-scoped micro-cache with request
// coalescing. It appears in the global janus options (default plus the
// process-wide pool knobs) and per site (override); unset keys cascade
// from the global settings, then built-in defaults. The full contract is
// docs/20260720-033201-capability-microcache.md.
type CacheSettings struct {
	// Enabled turns the cache on or off for the site. Default: off.
	// Sites may override the global default; explicit off beats an
	// inherited on.
	Enabled *bool `json:"enabled,omitempty"`

	// TTL is the freshness window for cached responses when the origin
	// sends no explicit lifetime. Default: 1s.
	TTL *caddy.Duration `json:"ttl,omitempty"`

	// TTLMax caps origin-declared lifetimes (s-maxage or max-age); a
	// larger declared value is clamped to this. Default: 10s.
	TTLMax *caddy.Duration `json:"ttl_max,omitempty"`

	// MaxBody is the largest response body, in bytes, the cache stores;
	// larger responses stream through uncached. Default: 262144 (256KiB).
	MaxBody *int64 `json:"max_body,omitempty"`

	// Debug adds an X-Janus-Cache header (HIT, MISS, BYPASS, …) to every
	// response served on the site. Default: off.
	Debug *bool `json:"debug,omitempty"`

	// MaxBytes is the process-wide memory pool shared by every site's
	// cache, in bytes. Global-only: illegal in a site block.
	// Default: 67108864 (64MiB).
	MaxBytes *int64 `json:"max_bytes,omitempty"`

	// MaxAppShare is the percentage of max_bytes one app may hold
	// (1–100). Global-only: illegal in a site block. Default: 50.
	MaxAppShare *int `json:"max_app_share,omitempty"`
}

// parseCacheDirective parses one cache directive:
//
//	cache
//	cache on
//	cache off
//	cache { ttl 1s; ttl_max 10s; max_body 256kb; debug; … }
//	cache on { … }
//
// Hard errors per the capability doc: unknown argument, a block on "off",
// unknown or duplicate subdirectives, invalid durations/sizes/percent,
// process-wide keys in a site block, nested blocks, debug with arguments.
func parseCacheDirective(d *caddyfile.Dispenser, global bool) (*CacheSettings, error) {
	cs := &CacheSettings{}
	on, err := parseOnOff(d.RemainingArgs())
	if err != nil {
		return nil, d.Errf("cache: %v", err)
	}
	cs.Enabled = &on

	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if !on {
			return nil, d.Err("cache off does not take a block (a block on an off switch is a contradiction)")
		}
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("cache: duplicate subdirective %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "ttl", "ttl_max":
			val, err := oneDirectiveArg(d, "cache", sub)
			if err != nil {
				return nil, err
			}
			dur, perr := caddy.ParseDuration(val)
			if perr != nil || dur <= 0 {
				return nil, d.Errf("cache %s: want a positive duration (e.g. 1s), got %q", sub, val)
			}
			cd := caddy.Duration(dur)
			if sub == "ttl" {
				cs.TTL = &cd
			} else {
				cs.TTLMax = &cd
			}
		case "max_body", "max_bytes":
			if sub == "max_bytes" && !global {
				return nil, d.Err("cache max_bytes names the process-wide pool; set it in the global janus block only")
			}
			val, err := oneDirectiveArg(d, "cache", sub)
			if err != nil {
				return nil, err
			}
			n, perr := humanize.ParseBytes(val)
			if perr != nil || n == 0 {
				return nil, d.Errf("cache %s: want a positive size (e.g. 256kb), got %q", sub, val)
			}
			v := int64(n)
			if sub == "max_body" {
				cs.MaxBody = &v
			} else {
				cs.MaxBytes = &v
			}
		case "max_app_share":
			if !global {
				return nil, d.Err("cache max_app_share is process-wide; set it in the global janus block only")
			}
			val, err := oneDirectiveArg(d, "cache", sub)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 1 || n > 100 {
				return nil, d.Errf("cache max_app_share: want an integer percent 1–100, got %q", val)
			}
			cs.MaxAppShare = &n
		case "debug":
			if len(d.RemainingArgs()) != 0 {
				return nil, d.Err("cache debug takes no arguments")
			}
			t := true
			cs.Debug = &t
		default:
			return nil, d.Errf("unrecognized cache subdirective: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("cache %s does not take a nested block", sub)
		}
	}
	return cs, nil
}

// --- cascade -----------------------------------------------------------------

func cacheEnabledPtr(cs *CacheSettings) *bool {
	if cs == nil {
		return nil
	}
	return cs.Enabled
}

// provisionCacheStore validates the process-wide knobs and builds the one
// pool. The store always exists — /1.0/cache counters are always on —
// even when no site enables the capability.
func (a *App) provisionCacheStore() error {
	maxBytes := int64(defaultCacheMaxBytes)
	share := defaultCacheAppShare
	if g := a.Cache; g != nil {
		if g.MaxBytes != nil {
			if *g.MaxBytes <= 0 {
				return fmt.Errorf("janus cache: max_bytes must be positive, got %d", *g.MaxBytes)
			}
			maxBytes = *g.MaxBytes
		}
		if g.MaxAppShare != nil {
			if *g.MaxAppShare < 1 || *g.MaxAppShare > 100 {
				return fmt.Errorf("janus cache: max_app_share must be 1–100, got %d", *g.MaxAppShare)
			}
			share = *g.MaxAppShare
		}
		if g.MaxBody != nil && *g.MaxBody <= 0 {
			return fmt.Errorf("janus cache: max_body must be positive, got %d", *g.MaxBody)
		}
		if g.TTL != nil && *g.TTL <= 0 {
			return fmt.Errorf("janus cache: ttl must be positive")
		}
		if g.TTLMax != nil && *g.TTLMax <= 0 {
			return fmt.Errorf("janus cache: ttl_max must be positive")
		}
		// Effective global pair (checked at provision, where both
		// effective values are known; sites re-check their own pair).
		ttl := cascadeDuration(nil, g.TTL, defaultCacheTTL)
		ttlMax := cascadeDuration(nil, g.TTLMax, defaultCacheTTLMax)
		if ttlMax < ttl {
			return fmt.Errorf("janus cache: ttl_max (%v) must be ≥ ttl (%v)", ttlMax, ttl)
		}
	}
	a.cache = newCacheStore(maxBytes, share)
	a.appsReg.setPurge(a.cache.purgeApp)
	return nil
}

// provisionCache resolves this site's effective cache configuration.
// Effective off leaves h.cacheCfg nil — the request-path check is one nil
// compare, per the hot-path rule.
func (h *Handler) provisionCache() error {
	var g *CacheSettings
	if h.app != nil {
		g = h.app.Cache
	}
	s := h.Cache
	if s != nil && (s.MaxBytes != nil || s.MaxAppShare != nil) {
		// The Caddyfile rejects this at parse; native JSON gets the same
		// rejection here.
		return fmt.Errorf("janus cache: max_bytes and max_app_share are process-wide; set them in the global janus block")
	}
	if s != nil {
		if s.MaxBody != nil && *s.MaxBody <= 0 {
			return fmt.Errorf("janus cache: max_body must be positive, got %d", *s.MaxBody)
		}
		if s.TTL != nil && *s.TTL <= 0 {
			return fmt.Errorf("janus cache: ttl must be positive")
		}
		if s.TTLMax != nil && *s.TTLMax <= 0 {
			return fmt.Errorf("janus cache: ttl_max must be positive")
		}
	}
	if !cascadeBool(cacheEnabledPtr(s), cacheEnabledPtr(g), false) {
		return nil
	}
	if h.app == nil || h.dp == nil {
		return nil // no registry/data plane to cache in front of
	}
	var sTTL, sTTLMax *caddy.Duration
	var sBody *int64
	var sDebug *bool
	if s != nil {
		sTTL, sTTLMax, sBody, sDebug = s.TTL, s.TTLMax, s.MaxBody, s.Debug
	}
	var gTTL, gTTLMax *caddy.Duration
	var gBody *int64
	var gDebug *bool
	if g != nil {
		gTTL, gTTLMax, gBody, gDebug = g.TTL, g.TTLMax, g.MaxBody, g.Debug
	}
	cc := &cacheSite{
		store:   h.app.cache,
		ttl:     cascadeDuration(sTTL, gTTL, defaultCacheTTL),
		ttlMax:  cascadeDuration(sTTLMax, gTTLMax, defaultCacheTTLMax),
		maxBody: cascadeInt64(sBody, gBody, defaultCacheMaxBody),
		debug:   cascadeBool(sDebug, gDebug, false),
	}
	if cc.ttlMax < cc.ttl {
		return fmt.Errorf("janus cache: effective ttl_max (%v) must be ≥ effective ttl (%v)", cc.ttlMax, cc.ttl)
	}
	h.cacheCfg = cc
	return nil
}
