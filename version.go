package janus

// The running janus version, for the surfaces that show it (the status
// page's footer). The janus binary publishes what `janus version` prints;
// a custom Caddy build that loads this module as a plugin publishes
// nothing, and the module's own version from the build info answers.

import (
	"runtime/debug"
	"strings"
	"sync/atomic"
)

const modulePath = "github.com/shreeve/janus"

var publishedVersion atomic.Value // string

// SetVersion publishes the version the binary reports.
func SetVersion(v string) {
	publishedVersion.Store(strings.TrimPrefix(strings.TrimSpace(v), "v"))
}

// Version is the published version, else this module's version in the
// build info, else "dev".
func Version() string {
	if v, _ := publishedVersion.Load().(string); v != "" {
		return v
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Path == modulePath && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			return strings.TrimPrefix(bi.Main.Version, "v")
		}
		for _, dep := range bi.Deps {
			if dep.Path == modulePath && dep.Version != "" {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "dev"
}
