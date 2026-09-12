package main

import (
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	p := servicePathsFor(false, "/home/ann", func(string) string { return "" })
	got := systemdUnitFile(p, "/home/ann/.local/bin/janus")
	for _, want := range []string{
		`ExecStart="/home/ann/.local/bin/janus" run --config "/home/ann/.config/janus/Caddyfile" --adapter caddyfile`,
		`Environment="HOME=/home/ann"`,
		`Environment="PATH=/home/ann/.local/bin:/usr/bin:/bin"`,
		"Restart=on-failure",
		"RestartSec=10",
		// A stop that Caddy cannot finish is ended here, not waited on.
		"TimeoutStopSec=15",
		"WantedBy=default.target",
		"StandardOutput=append:/home/ann/.local/state/janus/log/supervisor.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
	// A user unit ordered after default.target is an ordering cycle
	// (the target Wants= it, hence After= it) that drops the start job.
	if strings.Contains(got, "After=default.target") || strings.Contains(got, "Restart=always") {
		t.Errorf("unit carries a cycle or an always-restart:\n%s", got)
	}
	item := newServiceItem(p).(*systemdItem)
	if item.unit != "/home/ann/.config/systemd/user/janus.service" || !item.user {
		t.Errorf("user item: %+v", item)
	}
	// The drift-recovery loop must never give up: no start limit.
	if !strings.Contains(got, "[Unit]\nDescription=Janus edge\nStartLimitIntervalSec=0\n") {
		t.Errorf("user unit lacks StartLimitIntervalSec=0 in [Unit]:\n%s", got)
	}
	rp := servicePathsFor(true, "/root", func(string) string { return "" })
	rootUnit := systemdUnitFile(rp, "/usr/local/bin/janus")
	for _, want := range []string{
		"WantedBy=multi-user.target", "After=network-online.target", "Wants=network-online.target", `Environment="HOME=/var/lib/janus"`,
		"[Unit]\nDescription=Janus edge\nAfter=network-online.target\nWants=network-online.target\nStartLimitIntervalSec=0\n",
	} {
		if !strings.Contains(rootUnit, want) {
			t.Errorf("root unit lacks %q:\n%s", want, rootUnit)
		}
	}
	// The system edge runs as root, as its paths (/etc, /var) require; no
	// identity or sandboxing directive changes that under it.
	for _, no := range []string{"DynamicUser", "User=", "StateDirectory", "AmbientCapabilities", "NoNewPrivileges"} {
		if strings.Contains(rootUnit, no) || strings.Contains(got, no) {
			t.Errorf("unit carries %s:\n%s\n%s", no, rootUnit, got)
		}
	}
	root := newServiceItem(rp).(*systemdItem)
	if root.unit != "/etc/systemd/system/janus.service" || root.user {
		t.Errorf("root item: %+v", root)
	}
	t.Setenv("JANUS_SERVICE_LABEL", "janus-test7")
	alt := newServiceItem(p).(*systemdItem)
	if alt.service() != "janus-test7.service" || alt.unit != "/home/ann/.config/systemd/user/janus-test7.service" {
		t.Errorf("label override: %+v", alt)
	}
}

func TestParseSystemdShow(t *testing.T) {
	// Property order is the daemon's, not the -p order.
	state, pid := parseSystemdShow([]byte("MainPID=4242\nActiveState=active\n"))
	if state != "active" || pid != 4242 {
		t.Errorf("active: %q %d", state, pid)
	}
	state, pid = parseSystemdShow([]byte("ActiveState=inactive\nMainPID=0\n"))
	if state != "inactive" || pid != 0 {
		t.Errorf("inactive: %q %d", state, pid)
	}
}
