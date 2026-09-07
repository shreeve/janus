package main

// The service layer: the verbs that make one Janus edge per host start,
// stop, restart, and come back on its own, with the platform's session or
// system manager as the supervisor — a launchd item on macOS, a systemd unit
// on Linux. The vocabulary is Harbor's (start, stop, restart, autostart,
// autostart off, status), so an operator who learned one has learned both.
//
// The item runs `janus run --config <Caddyfile>` in the foreground, restarts
// it after a crash and never after a clean exit, so `janus stop` stays
// stopped until the next login (root: boot). Everything the edge needs
// lives under two roots: a config directory holding the Caddyfile, its
// drop-in sites, and an optional env file; and a state directory holding
// the control and admin sockets, the pidfile a bare `start` uses, and the
// process log the Caddyfile routes the default logger to.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
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

// localControlURL is the `control local` default the seed config enables.
// A variable so tests can point it at a port nothing answers on.
var localControlURL = "http://127.0.0.1:7600"

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
	# *.localhost, one label short of a real name). Nothing here tries to
	# install the CA into the system trust store (that would prompt, and
	# under the service there is no one to answer); 'janus trust' does.
	skip_install_trust
	on_demand_tls {
		ask http://127.0.0.1:7600/1.0/tls/ask
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

		# The /1.0 control API tenants register into: a unix socket for
		# processes on this host, and loopback HTTP.
		control internal %s
		control local

		# Directory listings with the embedded theme, for the roots apps
		# register and for 'janus serve'.
		browse

		# janus.local and every registered <name>.local, announced on the
		# LAN; that is how a phone finds a lan-mode edge by name.
		mdns
	}
}

# The network's local names: <name>.local, announced by mdns above. Apps
# register them; 'janus serve' opens a directory on them in lan mode. The
# edge's CA signs each name; 'janus trust' trusts it. This machine's own
# names, <name>.localhost, are the drop-in sites/localhost.caddy.
*.local {
	tls {
		issuer internal
		on_demand
	}
	janus {
		hub off
		auth off
	}
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
`, p.admin, p.log, p.sock, filepath.Join(filepath.Dir(p.log), "access.json"), p.sites)
}

// localhostSite is the drop-in that serves this machine's own names,
// <name>.localhost, with the edge's CA. It is a drop-in rather than a line
// in the Caddyfile because the Caddyfile may be another tool's to render
// (rip renders one from its templates); the sites directory is the
// operator's and survives every render, so janus can own this one file
// on any host. Written once; an edited copy is left alone.
func localhostSitePath(p servicePaths) string { return filepath.Join(p.sites, "localhost.caddy") }

func localhostSite() string {
	return `# This machine's local names, <name>.localhost: what 'janus serve' opens
# and what an app registers to be reached from this machine by name. The
# edge's own CA signs each name at its first handshake, gated by the
# Caddyfile's on_demand_tls ask ('janus trust' trusts the CA); browsers and
# the system resolver take *.localhost to mean this machine.
*.localhost {
	tls {
		issuer internal
		on_demand
	}
	janus {
		hub off
		auth off
	}
}
`
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
	return true, os.WriteFile(path, []byte(localhostSite()), 0o644)
}

// serviceItem is the supervisor-side half: one login item or system
// service for this host's edge. Registered means the file exists and the
// manager knows it; loaded means the manager holds the job now.
type serviceItem interface {
	// name is what the manager calls the job, for messages.
	name() string
	// file is the plist or unit path.
	file() string
	registered() bool
	// loaded reports whether the manager holds the job, and the pid of
	// the process it is running (0 when loaded but not running).
	loaded() (bool, int)
	// register writes the item and clears any disable the manager holds
	// for it. It does not load it. Reports whether the file changed.
	register(p servicePaths, exe string) (changed bool, err error)
	// load hands the item to the manager so the edge starts now, and at
	// login or boot from then on. A job the manager still holds from an
	// earlier file is replaced, so what runs is what the file says.
	load() error
	// unregister removes the item and leaves a running edge alone.
	// Reports whether there was one.
	unregister() (bool, error)
}

// serviceLabel is what the supervisor calls the edge: janus.edge under
// launchd, janus (janus.service) under systemd. JANUS_SERVICE_LABEL
// overrides it so a test host can run an edge beside the real one; pair it
// with XDG_CONFIG_HOME and XDG_STATE_HOME to move the files too.
func serviceLabel(def string) string {
	if v := os.Getenv("JANUS_SERVICE_LABEL"); v != "" {
		return v
	}
	return def
}

// itemFor is the platform's supervisor for a set of paths (nil where there
// is none); a variable so tests can stand in a fake.
var itemFor = newServiceItem

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

// --- verbs -------------------------------------------------------------------

func serviceCommands(caddyRun, caddyStart, caddyStop, caddyReload, caddyValidate, caddyAdapt, caddyTrust, caddyUntrust *cobra.Command) []*cobra.Command {
	p := currentPaths()

	wrap := func(cmd *cobra.Command, fn func(orig runFunc, cmd *cobra.Command, args []string) error) {
		orig := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error { return fn(orig, cmd, args) }
	}
	const serviceDefault = "\nThe service Caddyfile is the default --config when it exists.\n"

	// Caddy's own verbs learn the service's Caddyfile as their default.
	caddyRun.Long = janusify(caddyRun.Long) + `
With no Caddyfile in the current directory, the service Caddyfile is the
default when it exists, and the service env file is loaded beside it.
`
	wrap(caddyRun, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("config") {
			if _, err := os.Stat("Caddyfile"); err != nil && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
		}
		setEnvfile(cmd, p)
		st, err := setBindEnv(p)
		if err != nil {
			return err
		}
		if cfg, _ := cmd.Flags().GetString("config"); cfg == p.config {
			// The service edge: the mode must be enforceable here, the
			// host rule it needs must be in place before a wildcard
			// socket opens, nothing else may already answer on its
			// ports, and what it binds is watched against the mode for
			// as long as it runs.
			if err := serviceEdgeReady(st); err != nil {
				return err
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go exposureWatch(ctx, p)
		}
		return orig(cmd, args)
	})
	// adapt reads the Caddyfile too: the bind must be in its environment,
	// or {$JANUS_BIND} adapts to Caddy's own default, the wildcard.
	wrap(caddyAdapt, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if _, err := setBindEnv(p); err != nil {
			return err
		}
		return orig(cmd, args)
	})
	hiddenScopeFlags(caddyReload)
	for _, c := range []*cobra.Command{caddyReload, caddyValidate} {
		c.Use = strings.Replace(c.Use, "--config <path>", "[--config <path>]", 1)
		c.Long = janusify(c.Long) + serviceDefault
		wrap(c, func(orig runFunc, cmd *cobra.Command, args []string) error {
			if err := refuseScopeFlags(cmd); err != nil {
				return err
			}
			// Only the service edge is known to be running or not; a
			// --config or --address names some other edge.
			if cmd.Name() == "reload" && targetsService(cmd) && runningPID(p) == 0 && !controlReachable(p) {
				return errors.New("janus is not running; 'janus start' runs the current Caddyfile")
			}
			if !cmd.Flags().Changed("config") && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
			// The Caddyfile is adapted here, in this process: the service
			// env file and the mode's bind must be in the environment.
			if cfg, _ := cmd.Flags().GetString("config"); cfg == p.config {
				if err := loadEnvFile(p.env); err != nil {
					return err
				}
			}
			if _, err := setBindEnv(p); err != nil {
				return err
			}
			return orig(cmd, args)
		})
	}
	caddyStop.Long = janusify(caddyStop.Long) + serviceDefault + `
Under an autostart item this is a clean exit, so the edge stays stopped
until the next login (root: boot); 'janus start' brings it back. When
nothing is running this is a quiet no-op.
`
	wrap(caddyStop, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if targetsService(cmd) && runningPID(p) == 0 && !controlReachable(p) {
			fmt.Fprintln(cmd.OutOrStdout(), "janus was not running")
			return nil
		}
		if !cmd.Flags().Changed("config") && fileExists(p.config) {
			setConfig(cmd, p.config)
		}
		return orig(cmd, args)
	})

	// trust and untrust talk to the admin API, which the service edge keeps
	// on its own socket rather than Caddy's default port; when that socket
	// is there, it is the address.
	for _, c := range []*cobra.Command{caddyTrust, caddyUntrust} {
		c.Long = janusify(c.Long) + `
The running service edge's admin socket is the default --address.
`
		c.PreRunE = func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("address") && socketExists(p.admin) {
				return cmd.Flags().Set("address", "unix/"+p.admin)
			}
			return nil
		}
	}

	// start: under the item when there is one, else Caddy's background
	// start with the service's Caddyfile and pidfile as defaults.
	caddyStart.Long = `
Starts the edge in the background and returns.

With an autostart item installed ('janus autostart'), the item is what
runs the edge: this loads it, or starts it again after a 'janus stop'.
Start options belong to the item's Caddyfile then, and are refused here.

Without an item, this is Caddy's background start: the process detaches
and records its pid so 'janus status' and 'janus restart' can find it.
The service Caddyfile is the default --config when it exists, and the
service env file is loaded beside it.
`
	hiddenScopeFlags(caddyStart)
	wrap(caddyStart, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if err := refuseScopeFlags(cmd); err != nil {
			return err
		}
		return startEdge(orig, cmd, args, p)
	})

	restart := &cobra.Command{
		Use: "restart",
		Long: `
Stops the running edge gracefully, waits for it to exit (up to 10 s), and
starts it again, under its autostart item when there is one. Connections
drop for the moment in between; a config change alone is better served by
'janus reload', which keeps them.

This is how an installed upgrade takes effect: install the new binary,
then 'janus restart'.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return restartEdge(caddyStop, caddyStart, p, cmd)
		},
	}

	autostart := &cobra.Command{
		Use: "autostart [off [stop]]",
		Long: `
Installs the edge as a service under the platform's manager — a launchd
item on macOS, a systemd unit on Linux — and starts it now. From then on it
comes back after a crash and starts at every login; as root, at every boot
with no login needed. A clean 'janus stop' stays stopped until then.

Run as yourself on a machine you log in to. Run as root ('sudo janus
autostart', with janus installed where root finds it: /usr/local/bin) for
a server: ports 80 and 443 with no setcap, up before anyone logs in. One
or the other on a host, never both.

The service Caddyfile is seeded with a runnable starting point when absent
— the control plane, the admin socket, a rolling process log, and an
import of the drop-in sites directory beside it — and validated before
the item is installed, so a bad config never becomes a restart loop. An
env file beside it (KEY=value lines) is loaded into the edge at start.

'janus autostart off' removes the item and leaves a running edge alone;
'janus autostart off stop' takes both down. Run again after changing the
binary and the item is rewritten; 'janus restart' applies it.

--scope sets the exposure mode the edge is installed with (localhost, the
default when none is stored; lan takes --interface to pin one).
Without --scope a stored mode is kept. 'janus mode' changes it later.
`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return autostartEdge(caddyStop, p, args, cmd)
		},
	}
	addScopeFlags(autostart, true)

	status := &cobra.Command{
		Use: "status",
		Long: `
Shows the edge: running or stopped (and under what: the autostart item, a
pidfile, or nothing), this binary and its version, whether the binary is
newer than the edge that is running, the service Caddyfile, sites
directory, and log, and the control endpoint with how many apps are
registered on it.

Then the exposure mode: scope, the bind the Caddyfile gets through
JANUS_BIND, whether the host firewall rule the mode needs is in force
(root sees the firewall; run under sudo to verify it), and each front-door
address with how its reach is enforced — by the host (loopback, a private
address, a verified firewall) or only by the network.

Exits 3 when the edge is not running. --json prints the same as one
object, plus the service's paths for tools that write site files or
register with the edge: running, pid, uptime, supervisor, autostart,
loaded, binary, version, janus, caddy, binary_newer, config, sites, env,
state, socket (the control socket), admin (Caddy's admin socket), log,
scope, interface, bind, firewall, listeners, and control with apps
(present only when control answered).
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			sp, note := delegatedPaths()
			return statusEdge(sp, cmd, asJSON, note)
		},
	}
	status.Flags().Bool("json", false, "Print the status as JSON")

	return []*cobra.Command{restart, autostart, status, appsCommand(), logsCommand(), serveCommand(caddyReload), modeCommand(caddyReload), firewallCommand()}
}

type runFunc = func(cmd *cobra.Command, args []string) error

func setConfig(cmd *cobra.Command, path string) {
	_ = cmd.Flags().Set("config", path)
	if f := cmd.Flags().Lookup("adapter"); f != nil && !f.Changed {
		_ = cmd.Flags().Set("adapter", "caddyfile")
	}
}

// setEnvfile loads the service env file when the command runs the service
// Caddyfile and the operator named no env file of their own.
func setEnvfile(cmd *cobra.Command, p servicePaths) {
	f := cmd.Flags().Lookup("envfile")
	if f == nil || f.Changed || !fileExists(p.env) {
		return
	}
	if cfg, _ := cmd.Flags().GetString("config"); cfg == p.config {
		_ = cmd.Flags().Set("envfile", p.env)
	}
}

// targetsService reports whether a stop or reload is aimed at the service
// edge: no --config and no --address, which would name another one.
func targetsService(cmd *cobra.Command) bool {
	return !cmd.Flags().Changed("config") && !cmd.Flags().Changed("address")
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// --- start / restart -----------------------------------------------------------

func startEdge(orig runFunc, cmd *cobra.Command, args []string, p servicePaths) error {
	out := cmd.OutOrStdout()
	if item := itemFor(p); item != nil && item.registered() {
		for _, f := range []string{"config", "adapter", "envfile", "watch", "pidfile"} {
			if cmd.Flags().Changed(f) {
				return fmt.Errorf("start options belong to the autostart item, which runs the edge from %s: edit that file and 'janus reload', or 'janus autostart off' first", p.config)
			}
		}
		if _, pid := item.loaded(); pid > 0 && processAlive(pid) {
			fmt.Fprintf(out, "janus is already running (pid %d) under %s\n", pid, item.name())
			return nil
		}
		if st, err := readScope(p); err != nil {
			return err
		} else if err := serviceEdgeReady(st); err != nil {
			return err
		}
		if err := item.load(); err != nil {
			return err
		}
		fmt.Fprintf(out, "janus started under %s\n", item.name())
		return nil
	}
	if !cmd.Flags().Changed("config") {
		if fileExists(p.config) {
			setConfig(cmd, p.config)
		} else {
			// Caddy would start an empty instance; nothing here wants one.
			return fmt.Errorf("no Caddyfile at %s; 'janus autostart' seeds one, or pass --config", p.config)
		}
	}
	setEnvfile(cmd, p)
	if !cmd.Flags().Changed("pidfile") {
		if err := os.MkdirAll(p.run, 0o755); err == nil {
			_ = cmd.Flags().Set("pidfile", p.pid)
		}
	}
	return orig(cmd, args)
}

func restartEdge(caddyStop, caddyStart *cobra.Command, p servicePaths, cmd *cobra.Command) error {
	// The edge that comes back runs the service Caddyfile; a broken one
	// would be a restart loop under the item, and Caddy's stop reads it
	// too (for the admin address). Check before taking anything down.
	if fileExists(p.config) {
		if err := validateConfig(p.config); err != nil {
			return fmt.Errorf("%s does not validate; the edge is left as it is:\n%v", p.config, err)
		}
	}
	pid := runningPID(p)
	if pid > 0 {
		// A fresh flag set: the stop and start below must not inherit
		// anything the operator passed to restart (there is nothing to pass).
		if err := caddyStop.RunE(caddyStop, nil); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		if !waitGone(pid, 10*time.Second) {
			return fmt.Errorf("the edge (pid %d) did not exit within 10s", pid)
		}
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "janus was not running")
	}
	return caddyStart.RunE(caddyStart, nil)
}

// runningPID finds the edge's process: the item's, then the pidfile's.
func runningPID(p servicePaths) int {
	if item := itemFor(p); item != nil {
		if _, pid := item.loaded(); pid > 0 && processAlive(pid) {
			return pid
		}
	}
	return pidfilePID(p)
}

// pidfilePID reads the bare-start pidfile and vouches for it: the process
// must be alive and be a janus, or the file is stale (a crash leaves it
// behind, and pids get reused) and is removed.
func pidfilePID(p servicePaths) int {
	pid := readPidfile(p.pid)
	if pid <= 0 {
		return 0
	}
	if processAlive(pid) && isJanus(pid) {
		return pid
	}
	_ = os.Remove(p.pid)
	return 0
}

func readPidfile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// isJanus reports whether the process is a janus binary, by its command
// name as ps reports it.
func isJanus(pid int) bool {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(filepath.Base(strings.TrimSpace(string(out))), "janus")
}

func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// --- autostart -----------------------------------------------------------------

func autostartEdge(caddyStop *cobra.Command, p servicePaths, args []string, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	item := itemFor(p)
	if item == nil {
		return errors.New("autostart is only supported on macOS (launchd) and Linux (systemd)")
	}
	if len(args) > 0 {
		if args[0] != "off" || (len(args) == 2 && args[1] != "stop") || len(args) > 2 {
			return errors.New("usage: janus autostart [off [stop]] (to stop the edge and keep the item, 'janus stop')")
		}
		had, err := item.unregister()
		if err != nil {
			return err
		}
		if had {
			fmt.Fprintf(out, "autostart off: removed %s; a running edge is left alone\n", item.file())
		} else {
			fmt.Fprintln(out, "autostart was not on")
		}
		if len(args) == 2 {
			return caddyStop.RunE(caddyStop, nil)
		}
		return nil
	}

	// A unix socket path has a hard limit (104 bytes on macOS, 108 on
	// Linux) that validation cannot see: the listener binds at start,
	// which under a crash-only item is a restart loop.
	for _, sock := range []string{p.sock, p.admin} {
		if len(sock) > maxSocketPath {
			return fmt.Errorf("the socket path is %d bytes; unix sockets allow %d:\n  %s\nset XDG_STATE_HOME to a shorter directory", len(sock), maxSocketPath, sock)
		}
	}
	exe, err := installedExe(p)
	if err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(p.config), p.sites, p.run, filepath.Dir(p.log), filepath.Dir(p.sup)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if written, err := ensureLocalhostSite(p); err != nil {
		return err
	} else if written {
		fmt.Fprintf(out, "seeded %s\n", localhostSitePath(p))
	}
	prev, scope, err := autostartScope(cmd, p)
	if err != nil {
		return err
	}
	if err := writeScope(p, scope); err != nil {
		return err
	}
	if !fileExists(p.config) {
		if err := os.WriteFile(p.config, []byte(seedConfig(p)), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "seeded %s\n", p.config)
	}

	// Validate first: under a crash-only item a bad config is a restart
	// loop every ten seconds, forever.
	if err := validateConfig(p.config); err != nil {
		_ = writeScope(p, prev)
		return fmt.Errorf("%s does not validate; nothing installed:\n%v", p.config, err)
	}

	changed, err := item.register(p, exe)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "autostart on: %s\n", item.file())
	for _, warn := range platformNotes(p, exe) {
		fmt.Fprintln(out, warn)
	}
	// The host rule before the edge, so the wildcard socket on macOS is
	// never open unscoped.
	ready, err := autostartFirewall(cmd, p, scope)
	if err != nil {
		return err
	}

	_, pid := item.loaded()
	switch {
	case !ready:
		fmt.Fprintln(out, "not started: the edge refuses to serve until the rule is in place; then 'janus start'")
	case pid > 0 && processAlive(pid):
		if changed {
			fmt.Fprintf(out, "janus is running (pid %d) under the previous item; 'janus restart' applies the new one\n", pid)
		} else {
			fmt.Fprintf(out, "janus is already running (pid %d) under %s\n", pid, item.name())
		}
		if scope != prev {
			fmt.Fprintln(out, "the mode changed; 'janus reload' puts the running edge on it")
		}
	case runningPID(p) > 0 || controlReachable(p):
		fmt.Fprintln(out, "something already serves this edge outside the item; 'janus restart' hands it over")
	default:
		if err := foreignListener(scope, runtime.GOOS, dialTCP); err != nil {
			return err
		}
		if err := item.load(); err != nil {
			return err
		}
		fmt.Fprintf(out, "janus started under %s\n", item.name())
	}
	fmt.Fprintf(out, "config %s\nsites  %s\nlog    %s\n", p.config, p.sites, p.log)
	return nil
}

// autostartFirewall applies the mode's host firewall rule: directly with
// root, through 'sudo janus firewall' on a terminal, and otherwise it says
// what does and reports the edge not ready — it refuses to serve a scoped
// mode until the rule is in place.
func autostartFirewall(cmd *cobra.Command, p servicePaths, st scopeState) (ready bool, err error) {
	out := cmd.OutOrStdout()
	bind, err := st.bind(runtime.GOOS)
	if err != nil {
		return false, err
	}
	fmt.Fprintf(out, "scope  %s%s\nbind   %s\n", st.Scope, ifaceSuffix(st), strings.Join(bind, " "))
	if st.Scope == ScopeWAN {
		fmt.Fprintln(out, "wildcard exposure active: the edge answers on every interface")
	}
	if !NeedsFirewall(st.Scope, runtime.GOOS) {
		return true, nil
	}
	switch {
	case os.Geteuid() == 0:
		v, err := applyFirewall(hostFwOps(), st)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(out, "firewall %s\n", v)
	case canSudo():
		if err := sudoFirewall(cmd, "the firewall step for "+string(st.Scope)); err != nil {
			return false, err
		}
	case pfInstalled(hostFwOps(), st) == "":
		fmt.Fprintln(out, "firewall unverified (run: sudo janus status)")
	default:
		fmt.Fprintf(out, "firewall: %s is enforced by the host firewall, which needs root to load. Run:\n  sudo janus firewall\n", st.Scope)
		return false, nil
	}
	return true, nil
}

// serviceEdgeReady is what the service edge checks before it serves: the
// mode is one this OS enforces, the host rule a scoped mode needs is in
// place (the files; pf itself is root's to see), and nothing else answers
// on its ports.
func serviceEdgeReady(st scopeState) error {
	if err := platformExposure(servicePaths{}, st); err != nil {
		return err
	}
	if NeedsFirewall(st.Scope, runtime.GOOS) {
		if detail := pfInstalled(hostFwOps(), st); detail != "" {
			return fmt.Errorf("the host firewall for %s is not in place (%s); the edge refuses to open a wildcard socket unscoped. Run: sudo janus firewall", st.Scope, detail)
		}
	}
	return foreignListener(st, runtime.GOOS, dialTCP)
}

// installedExe is the binary the item runs: this one, by the path it was
// invoked as when that names the same file (a versioned install's stable
// symlink rather than the resolved target that vanishes on upgrade). A
// root item refuses a binary that someone other than root can change.
func installedExe(p servicePaths) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if len(os.Args) > 0 {
		if invoked, err := exec.LookPath(os.Args[0]); err == nil {
			if abs, err := filepath.Abs(invoked); err == nil {
				a, errA := os.Stat(abs)
				b, errB := os.Stat(exe)
				if errA == nil && errB == nil && os.SameFile(a, b) {
					exe = abs
				}
			}
		}
	}
	if p.root && !rootOwnedAndPrivate(exe) {
		return "", fmt.Errorf("%s is not root-owned and unwritable by others; a system service must not run a binary another user can change (install as root: 'curl -fsSL .../install.sh | sudo bash' puts it in /usr/local/bin)", exe)
	}
	return exe, nil
}

// validateConfig runs this binary's own 'janus validate' on the Caddyfile,
// quietly: Caddy narrates adapting and logger redirection on stderr, which
// only matters when the answer is no. A variable so tests can validate
// in-process (a test binary re-executed as 'validate' runs the tests).
var validateConfig = func(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := exec.Command(exe, "validate", "--config", path, "--adapter", "caddyfile").CombinedOutput()
	if err != nil {
		// Keep the verdict, drop the narration and the redundant prefix.
		var keep []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(line, "{") {
				continue
			}
			keep = append(keep, strings.TrimPrefix(line, "Error: "))
		}
		if len(keep) == 0 {
			keep = []string{err.Error()}
		}
		return errors.New(strings.Join(keep, "\n"))
	}
	return nil
}

// validateInProcess is what 'janus validate' does, without the process.
func validateInProcess(path string) error {
	if _, err := setBindEnv(currentPaths()); err != nil {
		return err
	}
	cfgJSON, _, _, err := caddycmd.LoadConfig(path, "caddyfile")
	if err != nil {
		return err
	}
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return err
	}
	return caddy.Validate(&cfg)
}

// --- status --------------------------------------------------------------------

// edgeStatus is what 'janus status' knows, in one value.
type edgeStatus struct {
	Running     bool   `json:"running"`
	PID         int    `json:"pid,omitempty"`
	Uptime      string `json:"uptime,omitempty"`
	Supervisor  string `json:"supervisor,omitempty"` // item name, or "pidfile"
	Autostart   bool   `json:"autostart"`            // the item file exists
	Loaded      bool   `json:"loaded"`               // the manager holds the job
	Binary      string `json:"binary"`
	Version     string `json:"version"`
	Janus       string `json:"janus"`
	Caddy       string `json:"caddy"`
	BinaryNewer bool   `json:"binary_newer"`
	Config      string `json:"config"`
	Sites       string `json:"sites"`
	Env         string `json:"env"`
	State       string `json:"state"`
	Socket      string `json:"socket"` // control internal socket
	Admin       string `json:"admin"`  // Caddy admin socket
	Log         string `json:"log"`
	Control     string `json:"control,omitempty"`
	Apps        *int   `json:"apps,omitempty"` // only when control answered
	// The exposure mode. Bind is what JANUS_BIND carries; Firewall is
	// verified / missing / unverified / none; Listeners are the front-door
	// addresses the mode admits, each with how its reach is enforced.
	Scope      string           `json:"scope"`
	Interface  string           `json:"interface,omitempty"`
	Bind       []string         `json:"bind"`
	Firewall   string           `json:"firewall"`
	Listeners  []listenerStatus `json:"listeners,omitempty"`
	ScopeError string           `json:"scope_error,omitempty"` // scope.json is unusable
}

type listenerStatus struct {
	Role  string `json:"role"`
	Addr  string `json:"addr"`
	Reach Reach  `json:"reach"`
}

func gatherStatus(p servicePaths) edgeStatus {
	st := edgeStatus{Config: p.config, Sites: p.sites, Env: p.env, State: p.state, Socket: p.sock, Admin: p.admin, Log: p.log, Version: versionLine(), Janus: janusVersion(), Caddy: caddyVersion()}
	st.Binary, _ = os.Executable()
	gatherExposure(&st, p)
	if item := itemFor(p); item != nil {
		st.Autostart = item.registered()
		st.Loaded, st.PID = item.loaded()
		if st.PID > 0 && !processAlive(st.PID) {
			st.PID = 0
		}
		if st.Loaded || st.Autostart {
			st.Supervisor = item.name()
		}
	}
	if st.PID == 0 {
		if fp := pidfilePID(p); fp > 0 {
			st.PID = fp
			st.Supervisor = "pidfile"
		}
	}
	if n, at := probeControl(p); at != "" {
		st.Control = at
		st.Apps = &n
	}
	st.Running = st.PID > 0 || st.Control != ""
	if st.PID > 0 {
		st.Uptime = elapsed(st.PID)
		st.BinaryNewer = binaryNewerThan(st.Binary, st.PID)
	}
	return st
}

// gatherExposure fills the mode fields: the stored scope, its bind on this
// OS, the firewall as far as this invocation can see it (root verifies;
// anyone else gets "unverified", never a claim), and the admitted
// addresses labeled by what enforces their reach.
func gatherExposure(st *edgeStatus, p servicePaths) {
	st.Bind = []string{}
	sc, err := readScope(p)
	if err != nil {
		st.ScopeError = err.Error()
		st.Firewall = "unknown (scope unusable)"
		return
	}
	st.Scope, st.Interface = string(sc.Scope), sc.Interface
	st.Bind, _ = sc.bind(runtime.GOOS)
	verdict := checkFirewallVerdict(sc)
	st.Firewall = verdict.String()
	plan, err := PlanListeners(sc.Scope, sc.lan())
	if err != nil {
		st.ScopeError = err.Error()
		return
	}
	for _, l := range plan {
		st.Listeners = append(st.Listeners, listenerStatus{Role: l.Role.String(), Addr: l.Addr.String(), Reach: classifyReach(l.Addr.Addr())})
	}
}

func statusEdge(p servicePaths, cmd *cobra.Command, asJSON bool, note string) error {
	out := cmd.OutOrStdout()
	st := gatherStatus(p)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			return err
		}
	} else {
		if note != "" {
			fmt.Fprintln(out, note)
		}
		printStatus(out, p, st)
	}
	if !st.Running {
		// Exit 3 (LSB "not running") for scripts; the output already
		// said it, so no error text.
		cmd.SilenceErrors = true
		return &exitError{code: 3, err: errors.New("janus is not running")}
	}
	return nil
}

func printStatus(out io.Writer, p servicePaths, st edgeStatus) {
	switch {
	case st.Running && st.PID > 0 && st.Uptime != "":
		fmt.Fprintf(out, "edge     running (pid %d, up %s)\n", st.PID, st.Uptime)
	case st.Running && st.PID > 0:
		fmt.Fprintf(out, "edge     running (pid %d)\n", st.PID)
	case st.Running:
		fmt.Fprintln(out, "edge     running (pid unknown: answered on control)")
	default:
		fmt.Fprintln(out, "edge     stopped")
	}
	switch {
	case st.Supervisor == "pidfile" && st.Autostart:
		fmt.Fprintf(out, "under    pidfile %s (autostart on, but the item is not running it: 'janus restart' hands it over)\n", p.pid)
	case st.Supervisor == "pidfile":
		fmt.Fprintf(out, "under    pidfile %s\n", p.pid)
	case st.Autostart && st.Loaded:
		fmt.Fprintf(out, "under    %s (autostart on)\n", st.Supervisor)
	case st.Autostart:
		fmt.Fprintf(out, "under    %s (autostart on, not loaded: 'janus start')\n", st.Supervisor)
	case st.Loaded:
		fmt.Fprintf(out, "under    %s (autostart off; the job stays until logout or 'janus stop')\n", st.Supervisor)
	default:
		fmt.Fprintln(out, "under    nothing (not under autostart)")
	}
	fmt.Fprintf(out, "binary   %s\nversion  %s\n", st.Binary, st.Version)
	if st.BinaryNewer {
		fmt.Fprintln(out, "         the binary is newer than the running edge: 'janus restart' to apply")
	}
	fmt.Fprintf(out, "config   %s\n", st.Config)
	fmt.Fprintf(out, "sites    %s\n", st.Sites)
	fmt.Fprintf(out, "log      %s\n", st.Log)
	switch {
	case st.Apps != nil:
		fmt.Fprintf(out, "control  %s (%d app%s registered)\n", st.Control, *st.Apps, plural(*st.Apps))
	case st.Running:
		fmt.Fprintf(out, "control  unreachable at %s and %s\n", p.sock, localControlURL)
	}
	printExposure(out, st)
}

func printExposure(out io.Writer, st edgeStatus) {
	if st.ScopeError != "" {
		fmt.Fprintf(out, "scope    UNUSABLE: %s\n", st.ScopeError)
		return
	}
	scope := st.Scope
	if st.Interface != "" {
		scope += " (" + st.Interface + ")"
	}
	fmt.Fprintf(out, "scope    %s\n", scope)
	bind := strings.Join(st.Bind, " ")
	if runtime.GOOS == "darwin" && st.Scope != string(ScopeWAN) {
		bind += "  (wildcard socket; pf scopes it)"
	}
	fmt.Fprintf(out, "bind     %s\n", bind)
	fmt.Fprintf(out, "firewall %s\n", st.Firewall)
	label := "https    "
	for _, l := range st.Listeners {
		if l.Role != "https" {
			continue
		}
		fmt.Fprintf(out, "%s%-24s %s\n", label, l.Addr, l.Reach)
		label = "         "
	}
	if len(st.Listeners) > 0 {
		fmt.Fprintln(out, "http     the same addresses on port 80")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// probeControl asks the edge's control plane for its apps: over the
// service's unix socket first (its path is this edge's alone), then over
// loopback HTTP, accepted only when the edge that answers says it also
// listens on that socket — any Janus with 'control local' answers on the
// port, and another one must not pass as this edge. Returns the count and
// the endpoint that answered, or "" when neither did.
func probeControl(p servicePaths) (int, string) {
	if socketExists(p.sock) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.sock)
		}
		if n, ok := fetchApps("http://janus/1.0/apps", dial); ok {
			return n, "unix " + p.sock
		}
	}
	if !controlListensOn(localControlURL+"/1.0", p.sock) {
		return 0, ""
	}
	if n, ok := fetchApps(localControlURL+"/1.0/apps", nil); ok {
		return n, localControlURL
	}
	return 0, ""
}

func controlReachable(p servicePaths) bool {
	_, at := probeControl(p)
	return at != ""
}

func socketExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode()&os.ModeSocket != 0
}

func controlClient(dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	tr := &http.Transport{}
	if dial != nil {
		tr.DialContext = dial
	}
	return &http.Client{Transport: tr, Timeout: 1500 * time.Millisecond}
}

func fetchApps(url string, dial func(context.Context, string, string) (net.Conn, error)) (int, bool) {
	resp, err := controlClient(dial).Get(url)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var apps []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apps); err != nil {
		return 0, false
	}
	return len(apps), true
}

// controlListensOn asks the control root at url whether that edge also
// listens on the unix socket path: the identity check for loopback.
func controlListensOn(url, sock string) bool {
	resp, err := controlClient(nil).Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var root struct {
		Type    string `json:"type"`
		Control []struct {
			Mode   string `json:"mode"`
			Listen string `json:"listen"`
		} `json:"control"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil || root.Type != "janus" {
		return false
	}
	for _, c := range root.Control {
		if c.Mode == "internal" && c.Listen == sock {
			return true
		}
	}
	return false
}

// elapsed reports how long the process has been up, as ps prints it.
func elapsed(pid int) string {
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// binaryNewerThan reports whether the executable on disk was modified
// after the process started: an installed upgrade the edge is not running.
func binaryNewerThan(exe string, pid int) bool {
	st, err := os.Stat(exe)
	if err != nil {
		return false
	}
	up := parseElapsed(elapsed(pid))
	if up <= 0 {
		return false
	}
	started := time.Now().Add(-up)
	return st.ModTime().After(started.Add(2 * time.Second))
}

// parseElapsed reads ps's etime: [[dd-]hh:]mm:ss.
func parseElapsed(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var secs int
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second
}

// runOut runs a command and returns its stdout, with stderr folded into
// the error.
func runOut(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
