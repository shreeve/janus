package main

// systemd: the edge is a user unit for a user (started with the session,
// or at boot once the account lingers) and a system unit for root.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func (s *systemdItem) register(p servicePaths, exe string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(s.unit), 0o755); err != nil {
		return false, err
	}
	body := []byte(systemdUnitFile(p, exe))
	prev, _ := os.ReadFile(s.unit)
	changed := !bytes.Equal(prev, body)
	if changed {
		if err := os.WriteFile(s.unit, body, 0o644); err != nil {
			return false, err
		}
	}
	if _, err := s.systemctl("daemon-reload"); err != nil {
		return false, err
	}
	_, err := s.systemctl("enable", s.service())
	return changed, err
}

// load starts the unit. A unit that hit its start limit refuses until
// reset; clear that first so a start after a fix takes.
func (s *systemdItem) load() error {
	_, _ = s.systemctl("reset-failed", s.service())
	_, err := s.systemctl("start", s.service())
	return err
}

// unregister disables and removes the unit; a running edge keeps running.
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
	unit, wanted := "", "default.target"
	if p.root {
		unit = "After=network-online.target\nWants=network-online.target\n"
		wanted = "multi-user.target"
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
%s
[Service]
Type=simple
ExecStart=%q run --config %q --adapter caddyfile
ExecReload=%q reload --config %q --adapter caddyfile
WorkingDirectory=%s
%sRestart=on-failure
RestartSec=10
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=%s
`, unit, exe, p.config, exe, p.config, p.state, envLines.String(), p.sup, p.sup, wanted)
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
