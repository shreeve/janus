package main

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
	// restart has the manager end the running edge, escalating to a kill
	// on its own timeout, and start it again from the item.
	restart() error
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
	out := cmd.OutOrStdout()
	pid := runningPID(p)
	// An edge the item runs is the manager's to restart: its stop is
	// bounded (SIGTERM, then SIGKILL on the item's timeout) and its start
	// is the same one boot performs, so the restart ends with an edge on
	// the current binary or with an error, never with a process that has
	// closed its listeners and is waiting on a connection that will not
	// end.
	if item := itemFor(p); item != nil && item.registered() {
		if loaded, ipid := item.loaded(); loaded && ipid > 0 && ipid == pid {
			if err := item.restart(); err != nil {
				return fmt.Errorf("restart under %s: %w", item.name(), err)
			}
			if !waitEdgeUp(p, 15*time.Second) {
				return fmt.Errorf("the edge did not answer within 15s of its restart under %s: 'janus status', and the log at %s", item.name(), p.log)
			}
			fmt.Fprintf(out, "janus restarted under %s\n", item.name())
			return nil
		}
	}
	// A bare edge (or a pidfile edge the item is about to take over): ask
	// it to stop, and end it if it will not.
	if pid > 0 || controlReachable(p) {
		// A fresh flag set: the stop and start below must not inherit
		// anything the operator passed to restart (there is nothing to pass).
		if err := caddyStop.RunE(caddyStop, nil); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		if !waitEdgeStopped(p, pid, 10*time.Second) {
			if err := forceStop(pid); err != nil {
				return err
			}
			fmt.Fprintf(out, "the edge did not stop within 10s; ended pid %d\n", pid)
		}
	} else {
		fmt.Fprintln(out, "janus was not running")
	}
	return caddyStart.RunE(caddyStart, nil)
}

// forceStop ends an edge that did not exit when asked: SIGTERM, then
// SIGKILL. Caddy without a bounded grace period waits for its last
// connection forever, and a hub socket never closes on its own; the seed
// bounds the wait, and this is the floor under an edge that runs another
// config.
func forceStop(pid int) error {
	if pid <= 0 {
		return errors.New("the edge did not stop within 10s and its pid is unknown: end it yourself, then 'janus start'")
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(syscall.SIGTERM)
	if waitGone(pid, 5*time.Second) {
		return nil
	}
	_ = proc.Kill()
	if waitGone(pid, 5*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d did not end on SIGKILL", pid)
}

// waitEdgeUp waits for the control plane to answer.
func waitEdgeUp(p servicePaths, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if controlReachable(p) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Foreground edges have no supervisor or pidfile. A restart must wait for
// their control listener to close before attempting to start a replacement.
func waitEdgeStopped(p servicePaths, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) && !controlReachable(p) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
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
