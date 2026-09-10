package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// servicePaths is where the service keeps its files, root-aware in the
// same way as the installer: a user's edge lives under the XDG directories,
// root's under /etc and /var.
type servicePaths struct {
	root   bool
	home   string // HOME for the supervised process (state root for root)
	config string // the Caddyfile
	sites  string // config/sites: drop-in *.caddy site files the Caddyfile imports
	env    string // config/env: KEY=value lines the edge starts with, when present
	state  string // state directory
	run    string // state/run: control socket, admin socket, pidfile
	log    string // the process log the Caddyfile writes
	sup    string // the supervisor's own capture of stdout/stderr
	pid    string // pidfile for a bare start
	sock   string // control internal socket
	admin  string // Caddy admin socket, so stop/reload can only ever reach this edge
	scope  string // state/scope.json: the exposure mode
	// uid and gid are the edge's owner when a verb runs as root on a
	// user's behalf (delegatedPaths); -1 when the verb runs as the owner.
	uid, gid int
}

func servicePathsFor(root bool, home string, getenv func(string) string) servicePaths {
	var p servicePaths
	p.root = root
	p.uid, p.gid = -1, -1
	if root {
		p.config = "/etc/janus/Caddyfile"
		p.state = "/var/lib/janus"
		p.log = "/var/log/janus/janus.log"
		p.sup = "/var/log/janus/supervisor.log"
		p.home = p.state
	} else {
		cfgHome := getenv("XDG_CONFIG_HOME")
		if cfgHome == "" {
			cfgHome = filepath.Join(home, ".config")
		}
		stateHome := getenv("XDG_STATE_HOME")
		if stateHome == "" {
			stateHome = filepath.Join(home, ".local", "state")
		}
		p.config = filepath.Join(cfgHome, "janus", "Caddyfile")
		p.state = filepath.Join(stateHome, "janus")
		p.log = filepath.Join(p.state, "log", "janus.log")
		p.sup = filepath.Join(p.state, "log", "supervisor.log")
		p.home = home
	}
	p.sites = filepath.Join(filepath.Dir(p.config), "sites")
	p.env = filepath.Join(filepath.Dir(p.config), "env")
	p.run = filepath.Join(p.state, "run")
	p.pid = filepath.Join(p.run, "janus.pid")
	p.sock = filepath.Join(p.run, "janus.sock")
	p.admin = filepath.Join(p.run, "admin.sock")
	p.scope = scopeFile(p)
	return p
}

func currentPaths() servicePaths {
	home, _ := os.UserHomeDir()
	return servicePathsFor(os.Geteuid() == 0, home, os.Getenv)
}

// maxSocketPath is the shorter of the platforms' sun_path limits, less
// the terminating NUL.
const maxSocketPath = 103

// seedConfig is the runnable Caddyfile `autostart` writes when none exists:
// the control plane apps register into, the admin socket the service verbs
// use, the process log on a rolling file, and an import of the drop-in
// sites directory — sites are the operator's (or their tools') to add.
func seedConfig(p servicePaths) string {
	return fmt.Sprintf(`# Janus — service config, seeded by 'janus autostart'.
#
# Edit freely: 'janus validate' checks it, 'janus reload' applies it to the
# running edge without dropping connections. The guided tour of every
# capability and knob is Caddyfile.example in the Janus repository:
# https://github.com/shreeve/janus
{
	# Caddy's admin API on a socket only this edge's user can reach.
	# 'janus stop', 'reload', and 'restart' talk to it; leave it on.
	admin unix/%s

	# The front door listens where the exposure mode says ('janus mode':
	# localhost, lan, or wan). Janus supplies JANUS_BIND from the stored
	# mode whenever it runs, reloads, validates, or adapts this file, and
	# the running edge checks its own sockets against the mode. No default
	# here on purpose: nothing but the stored mode fills it in.
	default_bind {$JANUS_BIND}

	# HTTP/1.1 and HTTP/2 only. HTTP/3 would open UDP listeners beside
	# the scoped TCP ones, which the exposure mode does not cover.
	servers {
		protocols h1 h2
	}

	# The local names below use the edge's own CA, one certificate per
	# name, minted at the first handshake for names registered with the
	# edge (a wildcard would not do: clients reject *.local and
	# *.localhost, one label short of a real name). Janus itself is the
	# permission: it answers from its registry, in-process. Nothing here
	# tries to install the CA into the system trust store (that would
	# prompt, and under the service there is no one to answer); 'janus
	# trust' does.
	skip_install_trust
	on_demand_tls {
		permission janus
	}

	# The process log: startup, reloads, certificate events, data-plane
	# warnings. Rolled so it never grows without bound. Access logs are
	# separate — each site names its own below.
	log {
		output file %s {
			roll_size 10MiB
			roll_keep 5
		}
	}

	janus {
		# Every site answers GET /ping with pong.
		ping

		# The /1.0 control API tenants register into: a unix socket, its
		# permissions this user's.
		control internal %s

		# Directory listings with the embedded theme, for the roots apps
		# register and for 'janus serve'.
		browse

		# The per-app WebSocket hub, admitted by the app (bridge) and
		# open to pages the edge itself served (origin same).
		hub {
			mode bridge
			origin same
		}

		# Serve a file's .br or .gz sibling when the client accepts it.
		files {
			precompressed
		}

		# janus.local and every registered <name>.local, announced on the
		# LAN; that is how a phone finds a lan-mode edge by name. Its
		# front door, http://janus.local, carries the status page and
		# /trust, the one-time setup that has a device trust the CA.
		mdns
	}
}

# The network's local names: <name>.local, announced by mdns above. Apps
# register them; 'janus serve' opens a directory on them in lan mode. The
# edge's CA signs each name; 'janus trust' trusts it here and
# http://janus.local/trust trusts it on a phone. This machine's own names,
# <name>.localhost, are the drop-in sites/localhost.caddy.
*.local {
	tls {
		issuer internal
		on_demand
	}
	log {
		output file %s
		format janus
	}
	janus
}

# mdns's shared front door: janus.local answers here with the status
# page; every other .local name is sent to HTTPS.
http://*.local {
	janus {
		auth off
		files off
		browse off
	}
}

# Sites live as drop-in files in the sites directory next to this file,
# one *.caddy per site (or write them here). A site that admits traffic
# into Janus carries a janus block; the app registers its hosts and worker
# sockets over /1.0 and Janus proxies to them. Caddy manages the
# certificate for every named host.
#
# app.example.com {
# 	log {
# 		output file %s
# 		format janus
# 	}
# 	janus
# }
import %s/*.caddy
`, p.admin, p.log, p.sock, accessLogPath(p), accessLogPath(p), p.sites)
}

// accessLogPath is the access log the seed's own sites write, beside the
// process log.
func accessLogPath(p servicePaths) string { return filepath.Join(filepath.Dir(p.log), "access.json") }

// localhostSite is the drop-in that serves this machine's own names,
// <name>.localhost, with the edge's CA. It is a drop-in rather than a line
// in the Caddyfile because the Caddyfile may be another tool's to render
// (rip renders one from its templates); the sites directory is the
// operator's and survives every render, so janus can own this one file
// on any host. Written once; an edited copy is left alone.
func localhostSitePath(p servicePaths) string { return filepath.Join(p.sites, "localhost.caddy") }

func localhostSite(p servicePaths) string {
	return fmt.Sprintf(`# This machine's local names, <name>.localhost: what 'janus serve' opens
# and what an app registers to be reached from this machine by name. The
# edge's own CA signs each name at its first handshake, for registered
# names only ('janus trust' trusts the CA); browsers and the system
# resolver take *.localhost to mean this machine.
*.localhost {
	tls {
		issuer internal
		on_demand
	}
	log {
		output file %s
		format janus
	}
	janus
}
`, accessLogPath(p))
}

// ensureLocalhostSite writes the drop-in when it is absent. Reports
// whether it wrote.
func ensureLocalhostSite(p servicePaths) (bool, error) {
	path := localhostSitePath(p)
	if fileExists(path) {
		return false, nil
	}
	if err := os.MkdirAll(p.sites, 0o755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(localhostSite(p)), 0o644)
}

// serviceEnv is the environment the item starts the edge with: HOME, a
// PATH of absolute entries that includes the binary's own directory, and
// any XDG roots the operator relocated, so Caddy's storage follows.
func serviceEnv(p servicePaths, exe string) map[string]string {
	env := map[string]string{"HOME": p.home}
	var path []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || !filepath.IsAbs(dir) || seen[dir] {
			return
		}
		seen[dir] = true
		path = append(path, dir)
	}
	add(filepath.Dir(exe))
	if p.root {
		for _, d := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
			add(d)
		}
	} else {
		for _, d := range filepath.SplitList(os.Getenv("PATH")) {
			add(d)
		}
	}
	env["PATH"] = strings.Join(path, string(os.PathListSeparator))
	if !p.root {
		for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
			if v := os.Getenv(k); v != "" {
				env[k] = v
			}
		}
	}
	return env
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
