package main

// systemd: the edge is a user unit for a user (started with the session,
// or at boot once the account is lingering) and a system unit for root.

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
	cmd := exec.Command("systemctl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

func (s *systemdItem) loaded() (bool, int) {
	out, err := s.systemctl("show", "-p", "ActiveState", "-p", "MainPID", "--value", s.service())
	if err != nil {
		return false, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return false, 0
	}
	// Order follows the -p flags: ActiveState, then MainPID.
	state, pidStr := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	pid, _ := strconv.Atoi(pidStr)
	switch state {
	case "active", "activating", "reloading":
		return true, pid
	case "deactivating", "failed", "inactive":
		return s.registered(), 0
	}
	return false, 0
}

func (s *systemdItem) register(p servicePaths, exe string) error {
	if err := os.MkdirAll(filepath.Dir(s.unit), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(s.unit, []byte(systemdUnitFile(p, exe)), 0o644); err != nil {
		return err
	}
	if _, err := s.systemctl("daemon-reload"); err != nil {
		return err
	}
	_, err := s.systemctl("enable", s.service())
	return err
}

func (s *systemdItem) load() error {
	_, err := s.systemctl("start", s.service())
	return err
}

func (s *systemdItem) kick() error { return s.load() }

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
	after, wanted := "network-online.target", "multi-user.target"
	if !p.root {
		after, wanted = "default.target", "default.target"
	}
	return fmt.Sprintf(`[Unit]
Description=Janus edge
After=%s

[Service]
Type=simple
ExecStart=%q run --config %q --adapter caddyfile
ExecReload=%q reload --config %q --adapter caddyfile
WorkingDirectory=%s
Environment=HOME=%s
Restart=on-failure
RestartSec=10
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=%s
`, after, exe, p.config, exe, p.config, p.state, p.home, p.sup, p.sup, wanted)
}

// lowPortWarning: a user's edge cannot bind 80 or 443 unless the binary
// carries the capability (or the sysctl allows it).
func lowPortWarning(p servicePaths, exe string) string {
	if p.root {
		return ""
	}
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n <= 80 {
			return ""
		}
	}
	if out, err := exec.Command("getcap", exe).Output(); err == nil && strings.Contains(string(out), "cap_net_bind_service") {
		return ""
	}
	return fmt.Sprintf("note: as a user this edge cannot bind ports 80/443; grant them with\n  sudo setcap cap_net_bind_service=+ep %s\nor run 'sudo janus autostart' for a system service. Ports 1024 and up work as is.", exe)
}
