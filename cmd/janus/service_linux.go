package main

// systemd: the edge is a user unit for a user (started with the session,
// or at boot once the account lingers) and a system unit for root.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const defaultSystemdLabel = "janus"

type systemdItem struct {
	user  bool
	label string // unit name without .service
	unit  string // unit file path
}

func newServiceItem(p servicePaths) serviceItem {
	label := serviceLabel(defaultSystemdLabel)
	if p.root {
		return &systemdItem{label: label, unit: "/etc/systemd/system/" + label + ".service"}
	}
	return &systemdItem{user: true, label: label, unit: filepath.Join(p.home, ".config", "systemd", "user", label+".service")}
}

func (s *systemdItem) service() string { return s.label + ".service" }

func (s *systemdItem) name() string {
	if s.user {
		return "systemd --user " + s.service()
	}
	return "systemd " + s.service()
}
func (s *systemdItem) file() string { return s.unit }

func (s *systemdItem) registered() bool { return fileExists(s.unit) }

func (s *systemdItem) systemctl(args ...string) ([]byte, error) {
	if s.user {
		args = append([]string{"--user"}, args...)
	}
	return runOut("systemctl", args...)
}

func (s *systemdItem) loaded() (bool, int) {
	out, err := s.systemctl("show", "-p", "ActiveState", "-p", "MainPID", s.service())
	if err != nil {
		return false, 0
	}
	state, pid := parseSystemdShow(out)
	switch state {
	case "active", "activating", "reloading":
		return true, pid
	case "deactivating", "failed", "inactive":
		return s.registered(), 0
	}
	return false, 0
}

// parseSystemdShow reads ActiveState and MainPID from `systemctl show`
// output, which is Key=Value lines in the daemon's order, not the -p order.
func parseSystemdShow(out []byte) (state string, pid int) {
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			state = strings.TrimSpace(v)
		case "MainPID":
			pid, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	return state, pid
}

// restartPending reports whether systemd will start the unit again: it
// waits out RestartSec as ActiveState activating, SubState auto-restart.
// A failed or inactive unit is not restarting.
func (s *systemdItem) restartPending() (bool, bool) {
	out, err := s.systemctl("show", "-p", "ActiveState", "-p", "SubState", s.service())
	if err != nil {
		return false, false
	}
	return parseSystemdRestartPending(out), true
}

func parseSystemdRestartPending(out []byte) bool {
	var active, sub string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "ActiveState":
			active = strings.TrimSpace(v)
		case "SubState":
			sub = strings.TrimSpace(v)
		}
	}
	return active == "activating" && sub == "auto-restart"
}

// lastExit is the status the unit's main process last exited with, from
// `systemctl show` (ExecMainStatus, with Result naming how it ended); not
// known before the unit's first exit.
func (s *systemdItem) lastExit() (int, bool) {
	out, err := s.systemctl("show", "-p", "Result", "-p", "ExecMainStatus", "-p", "ExecMainExitTimestampMonotonic", s.service())
	if err != nil {
		return 0, false
	}
	return parseSystemdLastExit(out)
}

func parseSystemdLastExit(out []byte) (int, bool) {
	var result string
	var status, exited int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Result":
			result = v
		case "ExecMainStatus":
			status, _ = strconv.Atoi(v)
		case "ExecMainExitTimestampMonotonic":
			exited, _ = strconv.Atoi(v)
		}
	}
	if exited == 0 {
		return 0, false // never exited
	}
	if status == 0 && result != "" && result != "success" {
		return -1, true // ended by a signal, a timeout, or a watchdog
	}
	return status, true
}

func (s *systemdItem) register(p servicePaths, exe string) (bool, error) {
	changed, err := writeItemFile(s.unit, []byte(systemdUnitFile(p, exe)))
	if err != nil {
		return false, err
	}
	if _, err := s.systemctl("daemon-reload"); err != nil {
		return false, err
	}
	_, err = s.systemctl("enable", s.service())
	return changed, err
}

// load starts the unit. A unit that hit its start limit refuses until
// reset; clear that first so a start after a fix takes.
func (s *systemdItem) load() error {
	_, _ = s.systemctl("reset-failed", s.service())
	_, err := s.systemctl("start", s.service())
	return err
}

// restart is systemd's own: SIGTERM, SIGKILL at TimeoutStopSec, then the
// unit starts again on the binary the file names. A hit start limit is
// cleared first, as in load.
func (s *systemdItem) restart() error {
	_, _ = s.systemctl("reset-failed", s.service())
	_, err := s.systemctl("restart", s.service())
	return err
}

// unregister disables and removes the unit; a running edge keeps running.
// exe reads the executable from the unit's ExecStart line.
func (s *systemdItem) exe() string {
	b, err := os.ReadFile(s.unit)
	if err != nil {
		return ""
	}
	return parseSystemdExe(b)
}

// parseSystemdExe returns the first word of ExecStart, which the unit
// writer quotes with %q.
func parseSystemdExe(unit []byte) string {
	for _, line := range strings.Split(string(unit), "\n") {
		rest, ok := strings.CutPrefix(line, "ExecStart=")
		if !ok {
			continue
		}
		quoted, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return ""
		}
		exe, err := strconv.Unquote(quoted)
		if err != nil {
			return ""
		}
		return exe
	}
	return ""
}

// relaunch restarts the unit. register has already reloaded the manager,
// so the restart runs the file as it now is; `start`, which load uses, is
// a no-op on a unit that is already active and would leave the old edge
// running.
func (s *systemdItem) relaunch() error { return s.restart() }

func (s *systemdItem) unregister() (bool, error) {
	if !s.registered() {
		return false, nil
	}
	_, _ = s.systemctl("disable", s.service())
	if err := os.Remove(s.unit); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, err
	}
	_, _ = s.systemctl("daemon-reload")
	return true, nil
}

func systemdUnitFile(p servicePaths, exe string) string {
	// A user unit must not order itself after default.target: the target
	// pulls the unit in with Wants=, which implies After=janus.service,
	// and the pair is an ordering cycle systemd resolves by dropping the
	// unit's start job at login.
	//
	// StartLimitIntervalSec=0: a lan edge whose interface address moved
	// cannot bind and exits; it must keep retrying every ten seconds until
	// the address is back or 'janus mode lan' stores the new one. systemd's
	// default burst limit would give up after five tries and leave it down.
	unit, wanted, sandbox := "", "default.target", ""
	if p.root {
		unit = "After=network-online.target\nWants=network-online.target\n"
		wanted = "multi-user.target"
		sandbox = rootSandbox(p)
	}
	env := serviceEnv(p, exe)
	var envLines strings.Builder
	for _, k := range []string{"HOME", "PATH", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		if v, ok := env[k]; ok {
			fmt.Fprintf(&envLines, "Environment=%q\n", k+"="+v)
		}
	}
	return fmt.Sprintf(`[Unit]
Description=Janus edge
%sStartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=%q run --config %q --adapter caddyfile
ExecReload=%q reload --config %q --adapter caddyfile
WorkingDirectory=%s
%sRestart=on-failure
RestartSec=10
TimeoutStopSec=15
StandardOutput=append:%s
StandardError=append:%s
%s
[Install]
WantedBy=%s
`, unit, exe, p.config, exe, p.config, p.state, envLines.String(), p.sup, p.sup, sandbox, wanted)
}

// rootSandbox is the system unit's confinement. The edge stays root (its
// paths are root's, and no User= changes that); what it may do as root
// narrows to what the documented layout and its tenants need:
//
//   - NoNewPrivileges: nothing it runs (a browse renderer) gains
//     privilege through a setuid bit or a file capability.
//   - ProtectSystem=full: /usr, /boot, and /etc are read-only. The edge
//     reads /etc/janus and never writes it; the verbs that write there
//     (autostart, serve, mode) run outside the unit.
//   - ProtectHome=read-only: the edge reads registered roots under /home
//     and connects to tenant sockets there (a socket on a read-only mount
//     still accepts connections); it writes nothing there.
//   - ReadWritePaths: its state (Caddy's storage and autosave, sockets,
//     scope) and its logs, writable whatever else is confined; "-" lets a
//     directory not created yet be absent.
//   - CapabilityBoundingSet: CAP_NET_BIND_SERVICE for ports 80 and 443,
//     and CAP_DAC_OVERRIDE, without which root could not reach a
//     tenant's worker sockets in a directory only that tenant may enter
//     (Rip's are 0700 under /tmp) or read a root a tenant registers.
//
// PrivateTmp is left off on purpose: tenants put their worker and
// doorbell sockets under /tmp, and a private /tmp would hide them. A
// user unit carries none of this: NoNewPrivileges would void the
// cap_net_bind_service a user's binary is given with setcap.
func rootSandbox(p servicePaths) string {
	rw := []string{"-" + p.state}
	for _, dir := range []string{filepath.Dir(p.log), filepath.Dir(p.sup)} {
		if d := "-" + dir; !slices.Contains(rw, d) {
			rw = append(rw, d)
		}
	}
	return "NoNewPrivileges=yes\n" +
		"ProtectSystem=full\n" +
		"ProtectHome=read-only\n" +
		"ReadWritePaths=" + strings.Join(rw, " ") + "\n" +
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_DAC_OVERRIDE\n"
}

// platformNotes: a user's edge cannot bind 80 or 443 unless the binary
// carries the capability (or the sysctl allows it), and it stops at logout
// unless the account lingers, which is asked for here.
func platformNotes(p servicePaths, exe string) []string {
	if p.root {
		return nil
	}
	var notes []string
	if out, err := runOut("loginctl", "enable-linger"); err != nil {
		notes = append(notes, "note: 'loginctl enable-linger' failed ("+strings.TrimSpace(err.Error()+" "+string(out))+"); without it the edge stops at logout and does not start at boot")
	}
	lowPorts := false
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n <= 80 {
			lowPorts = true
		}
	}
	if !lowPorts {
		if _, err := exec.LookPath("getcap"); err != nil {
			notes = append(notes, "note: could not check for cap_net_bind_service (no getcap); as a user the edge needs it to bind ports 80/443")
		} else if out, err := exec.Command("getcap", exe).Output(); err == nil && strings.Contains(string(out), "cap_net_bind_service") {
			lowPorts = true
		}
	}
	if !lowPorts {
		notes = append(notes, fmt.Sprintf("note: as a user this edge cannot bind ports 80/443; grant them with\n  sudo setcap cap_net_bind_service=+ep %s\nor run 'sudo janus autostart' for a system service. Ports 1024 and up work as is.", exe))
	}
	return notes
}
