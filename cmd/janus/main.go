// Command janus is the Janus edge binary: stock Caddy compiled together
// with the Janus module and the Route 53 DNS provider (DNS-01 wildcard
// issuance) as one static executable named janus.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddy-dns/route53"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/shreeve/janus"
)

// version is stamped by release builds (-ldflags "-X main.version=$tag");
// source builds fall back to the VCS-stamped module version.
var version string

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "-V", "--version":
			fmt.Printf("janus %s (caddy %s)\n", janusVersion(), caddyVersion())
			return
		case "upgrade", "add-package", "remove-package":
			// These subcommands operate on stock Caddy binaries downloaded
			// from caddyserver.com; on a Janus build they have nothing to
			// manage, and Caddy's package machinery dereferences module
			// metadata that exists only for dependency-built plugins.
			fmt.Fprintf(os.Stderr, "janus: %q applies to stock Caddy binaries; rebuild Janus from source to change modules\n", os.Args[1])
			os.Exit(1)
		}
	}
	caddy.CustomBinaryName = "janus"
	caddycmd.Main()
}

// janusVersion reports the release tag without the leading v, preferring
// the ldflags stamp over the toolchain's VCS-derived module version.
func janusVersion() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}

// caddyVersion reports the compiled Caddy dependency version without the
// leading v.
func caddyVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/caddyserver/caddy/v2" {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "unknown"
}
