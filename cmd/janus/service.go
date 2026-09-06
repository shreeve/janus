package main

// The service layer: the verbs that make one Janus edge per host start,
// stop, restart, and come back on its own, with the platform's session or
// system manager as the supervisor — a launchd item on macOS, a systemd unit
// on Linux. The vocabulary is Harbor's (start, stop, restart, autostart,
// autostart off, status), so an operator who learned one has learned both.
//
// The item runs `janus run --config <Caddyfile>` in the foreground, restarts
// it after a crash and never after a clean exit, so `janus stop` stays
// stopped until the next boot. Everything the edge needs lives under two
// roots: a config directory holding the Caddyfile, and a state directory
// holding the control socket, the pidfile a bare `start` uses, and the
// process log the Caddyfile routes the default logger to.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	state  string // state directory
	run    string // state/run: control socket, pidfile
	log    string // the process log the Caddyfile writes
	sup    string // the supervisor's own capture of stdout/stderr
	pid    string // pidfile for a bare start
	sock   string // control internal socket
}

func servicePathsFor(root bool, home string, getenv func(string) string) servicePaths {
	var p servicePaths
	p.root = root
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
	p.run = filepath.Join(p.state, "run")
	p.pid = filepath.Join(p.run, "janus.pid")
	p.sock = filepath.Join(p.run, "janus.sock")
	return p
}

func currentPaths() servicePaths {
	home, _ := os.UserHomeDir()
	return servicePathsFor(os.Geteuid() == 0, home, os.Getenv)
}

// localControl is the `control local` default the seed config enables.
const localControl = "http://127.0.0.1:7600"

// seedConfig is the runnable Caddyfile `autostart` writes when none exists:
// the control plane apps register into, the process log on a rolling
// file, and no sites — those are the operator's to add.
func seedConfig(p servicePaths) string {
	return fmt.Sprintf(`# Janus — service config, seeded by 'janus autostart'.
#
# Edit freely: 'janus validate' checks it, 'janus reload' applies it to the
# running edge without dropping connections. The guided tour of every
# capability and knob is Caddyfile.example in the Janus repository:
# https://github.com/shreeve/janus
{
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
	}
}

# Sites go here. A site that admits traffic into Janus carries a janus
# block; the app registers its hosts and worker sockets over /1.0 and Janus
# proxies to them. Caddy manages the certificate for every named host.
#
# app.example.com {
# 	log {
# 		output file %s
# 		format janus
# 	}
# 	janus
# }
`, p.log, p.sock, filepath.Join(filepath.Dir(p.log), "access.json"))
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
	// for it. It does not load it.
	register(p servicePaths, exe string) error
	// load hands the item to the manager: the edge starts now, and at
	// boot from then on.
	load() error
	// kick starts a loaded item whose process has exited.
	kick() error
	// unregister removes the item and leaves a running edge alone.
	// Reports whether there was one.
	unregister() (bool, error)
}

// itemFor is the platform's supervisor for a set of paths (nil where there
// is none); a variable so tests can stand in a fake.
var itemFor = newServiceItem

// --- verbs -------------------------------------------------------------------

func serviceCommands(caddyRun, caddyStart, caddyStop, caddyReload, caddyValidate *cobra.Command) []*cobra.Command {
	p := currentPaths()

	wrap := func(cmd *cobra.Command, fn func(orig runFunc, cmd *cobra.Command, args []string) error) {
		orig := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error { return fn(orig, cmd, args) }
	}

	// Caddy's own verbs learn the service's Caddyfile as their default.
	wrap(caddyRun, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("config") {
			if _, err := os.Stat("Caddyfile"); err != nil && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
		}
		return orig(cmd, args)
	})
	for _, c := range []*cobra.Command{caddyStop, caddyReload, caddyValidate} {
		wrap(c, func(orig runFunc, cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("config") && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
			return orig(cmd, args)
		})
	}
	caddyStop.Long = janusify(caddyStop.Long) + `
Under an autostart item this is a clean exit, so the edge stays stopped
until the next boot; 'janus start' or 'janus autostart' brings it back.
`
	// start: under the item when there is one, else Caddy's background
	// start with the service's Caddyfile and pidfile as defaults.
	caddyStart.Short = "Starts the Janus process in the background — under its autostart item when one is installed"
	caddyStart.Long = `
Starts the edge in the background and returns.

With an autostart item installed ('janus autostart'), the item is what
runs the edge: this loads it, or starts it again after a 'janus stop'.
Start options belong to the item's Caddyfile then, and are refused here.

Without an item, this is Caddy's background start: the process detaches
and records its pid so 'janus status' and 'janus restart' can find it.
The service Caddyfile is the default --config when it exists.
`
	wrap(caddyStart, func(orig runFunc, cmd *cobra.Command, args []string) error {
		return startEdge(orig, cmd, args, p)
	})

	restart := &cobra.Command{
		Use:   "restart",
		Short: "Stops the edge and starts it again — the upgrade path",
		Long: `
Stops the running edge gracefully and starts it again, under its autostart
item when there is one. Connections drop for the moment in between; a
config change alone is better served by 'janus reload', which keeps them.

This is how an installed upgrade takes effect: install the new binary,
then 'janus restart'.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return restartEdge(caddyStop, caddyStart, p, cmd)
		},
	}

	autostart := &cobra.Command{
		Use:   "autostart [off [stop]]",
		Short: "Keeps the edge running: now, at every boot, and again after a crash",
		Long: `
Installs the edge as a service under the platform's manager — a launchd
item on macOS, a systemd unit on Linux — and starts it now. From then on it
starts at boot and comes back after a crash. A clean 'janus stop' stays
stopped until the next boot.

Run as a user the item is a login item (~/Library/LaunchAgents, or a
systemd user unit); run as root it is a system service (/Library/LaunchDaemons,
or /etc/systemd/system) that needs no login session.

The Caddyfile is the service config: ` + "`--config`" + ` names one, else the default
location is used and seeded with a runnable starting point when absent. It
is validated before the item is installed, so a bad config never becomes a
restart loop.

'janus autostart off' removes the item and leaves a running edge alone;
'janus autostart off stop' takes both down.
`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _ := cmd.Flags().GetString("config")
			return autostartEdge(caddyStop, p, cfg, args, cmd)
		},
	}
	autostart.Flags().String("config", "", "Caddyfile the item runs (default: the service config, seeded when absent)")

	status := &cobra.Command{
		Use:   "status",
		Short: "Shows the edge: running or not, under what, which binary, and how many apps are registered",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return statusEdge(p, cmd)
		},
	}

	return []*cobra.Command{restart, autostart, status}
}

type runFunc = func(cmd *cobra.Command, args []string) error

func setConfig(cmd *cobra.Command, path string) {
	_ = cmd.Flags().Set("config", path)
	if f := cmd.Flags().Lookup("adapter"); f != nil && !f.Changed {
		_ = cmd.Flags().Set("adapter", "caddyfile")
	}
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// --- start / restart -----------------------------------------------------------

func startEdge(orig runFunc, cmd *cobra.Command, args []string, p servicePaths) error {
	if item := itemFor(p); item != nil && item.registered() {
		for _, f := range []string{"config", "adapter", "envfile", "watch", "pidfile"} {
			if cmd.Flags().Changed(f) {
				return fmt.Errorf("an autostart item runs the edge from %s; edit that file and 'janus reload', or 'janus autostart off' first", p.config)
			}
		}
		loaded, pid := item.loaded()
		switch {
		case loaded && pid > 0:
			fmt.Fprintf(cmd.OutOrStdout(), "janus is already running (pid %d) under %s\n", pid, item.name())
			return nil
		case loaded:
			if err := item.kick(); err != nil {
				return err
			}
		default:
			if err := item.load(); err != nil {
				return err
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "janus started under %s\n", item.name())
		return nil
	}
	if !cmd.Flags().Changed("config") && fileExists(p.config) {
		setConfig(cmd, p.config)
	}
	if !cmd.Flags().Changed("pidfile") {
		if err := os.MkdirAll(p.run, 0o755); err == nil {
			_ = cmd.Flags().Set("pidfile", p.pid)
		}
	}
	return orig(cmd, args)
}

func restartEdge(caddyStop, caddyStart *cobra.Command, p servicePaths, cmd *cobra.Command) error {
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
		if _, pid := item.loaded(); pid > 0 {
			return pid
		}
	}
	if pid := readPidfile(p.pid); pid > 0 && processAlive(pid) {
		return pid
	}
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

func autostartEdge(caddyStop *cobra.Command, p servicePaths, cfg string, args []string, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	item := itemFor(p)
	if item == nil {
		return errors.New("autostart is only supported on macOS (launchd) and Linux (systemd)")
	}
	if len(args) > 0 {
		if args[0] != "off" || (len(args) == 2 && args[1] != "stop") || len(args) > 2 {
			return errors.New("usage: janus autostart [off [stop]]")
		}
		had, err := item.unregister()
		if err != nil {
			return err
		}
		if had {
			fmt.Fprintf(out, "autostart off: removed %s\n", item.file())
		} else {
			fmt.Fprintln(out, "autostart was not on")
		}
		if len(args) == 2 {
			if runningPID(p) == 0 && !controlReachable(p) {
				fmt.Fprintln(out, "janus was not running")
				return nil
			}
			return caddyStop.RunE(caddyStop, nil)
		}
		return nil
	}

	if cfg != "" {
		abs, err := filepath.Abs(cfg)
		if err != nil {
			return err
		}
		p.config = abs
		if !fileExists(p.config) {
			return fmt.Errorf("no Caddyfile at %s", p.config)
		}
	}
	for _, dir := range []string{filepath.Dir(p.config), p.run, filepath.Dir(p.log), filepath.Dir(p.sup)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
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
		return fmt.Errorf("%s does not validate; nothing installed:\n%v", p.config, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	if err := item.register(p, exe); err != nil {
		return err
	}
	fmt.Fprintf(out, "autostart on: %s\n", item.file())
	if warn := lowPortWarning(p, exe); warn != "" {
		fmt.Fprintln(out, warn)
	}

	loaded, pid := item.loaded()
	switch {
	case loaded && pid > 0:
		fmt.Fprintf(out, "janus is already running (pid %d) under %s\n", pid, item.name())
	case runningPID(p) > 0 || controlReachable(p):
		fmt.Fprintln(out, "something already serves this edge outside the item; 'janus restart' hands it over")
	case loaded:
		if err := item.kick(); err != nil {
			return err
		}
		fmt.Fprintf(out, "janus started under %s\n", item.name())
	default:
		if err := item.load(); err != nil {
			return err
		}
		fmt.Fprintf(out, "janus started under %s\n", item.name())
	}
	fmt.Fprintf(out, "config %s\nlog    %s\n", p.config, p.log)
	return nil
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

func statusEdge(p servicePaths, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	item := itemFor(p)
	var under string
	pid := 0
	if item != nil && item.registered() {
		loaded, ipid := item.loaded()
		pid = ipid
		switch {
		case loaded:
			under = item.name() + " (autostart on)"
		default:
			under = item.name() + " (autostart on, not loaded — 'janus start')"
		}
	}
	if pid == 0 {
		if fp := readPidfile(p.pid); fp > 0 && processAlive(fp) {
			pid = fp
			if under == "" {
				under = "pidfile " + p.pid
			}
		}
	}
	apps, control := probeControl(p)
	running := pid > 0 || control != ""
	if under == "" {
		under = "nothing — not under autostart"
	}

	if running {
		up := ""
		if pid > 0 {
			up = elapsed(pid)
		}
		switch {
		case pid > 0 && up != "":
			fmt.Fprintf(out, "edge     running (pid %d, up %s)\n", pid, up)
		case pid > 0:
			fmt.Fprintf(out, "edge     running (pid %d)\n", pid)
		default:
			fmt.Fprintln(out, "edge     running (pid unknown: answered on control)")
		}
	} else {
		fmt.Fprintln(out, "edge     stopped")
	}
	fmt.Fprintf(out, "under    %s\n", under)
	exe, _ := os.Executable()
	fmt.Fprintf(out, "binary   %s\nversion  %s\n", exe, versionLine())
	if pid > 0 && binaryNewerThan(exe, pid) {
		fmt.Fprintln(out, "         the binary is newer than the running edge: 'janus restart' to apply")
	}
	fmt.Fprintf(out, "config   %s\n", p.config)
	fmt.Fprintf(out, "log      %s\n", p.log)
	switch {
	case control != "":
		fmt.Fprintf(out, "control  %s (%d app%s registered)\n", control, apps, plural(apps))
	case running:
		fmt.Fprintf(out, "control  unreachable at %s and %s\n", localControl, p.sock)
	}
	if !running {
		// Exit 3 (LSB "not running") for scripts; the lines above already
		// said it, so no error text.
		cmd.SilenceErrors = true
		return &exitError{code: 3, err: errors.New("janus is not running")}
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// probeControl asks the edge's control plane for its apps, over loopback
// HTTP first and the service's unix socket second. Returns the count and
// the endpoint that answered, or "" when neither did.
func probeControl(p servicePaths) (int, string) {
	if n, ok := fetchApps(localControl+"/1.0/apps", nil); ok {
		return n, localControl
	}
	if fileExists(p.sock) || socketExists(p.sock) {
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.sock)
		}
		if n, ok := fetchApps("http://janus/1.0/apps", dial); ok {
			return n, "unix " + p.sock
		}
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

func fetchApps(url string, dial func(context.Context, string, string) (net.Conn, error)) (int, bool) {
	tr := &http.Transport{}
	if dial != nil {
		tr.DialContext = dial
	}
	client := &http.Client{Transport: tr, Timeout: 1500 * time.Millisecond}
	resp, err := client.Get(url)
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
