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
	// identity directive changes that under it.
	for _, no := range []string{"DynamicUser", "User=", "StateDirectory", "AmbientCapabilities"} {
		if strings.Contains(rootUnit, no) || strings.Contains(got, no) {
			t.Errorf("unit carries %s:\n%s\n%s", no, rootUnit, got)
		}
	}
	// Root's edge is confined to what the documented layout needs: its
	// state and logs writable, /etc and /usr read-only, /home readable,
	// and the capabilities for the low ports and for tenant sockets.
	sandbox := "StandardError=append:/var/log/janus/supervisor.log\n" +
		"NoNewPrivileges=yes\n" +
		"ProtectSystem=full\n" +
		"ProtectHome=read-only\n" +
		"ReadWritePaths=-/var/lib/janus -/var/log/janus\n" +
		"CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_DAC_OVERRIDE\n" +
		"\n[Install]\n"
	if !strings.Contains(rootUnit, sandbox) {
		t.Errorf("root unit lacks its sandbox %q:\n%s", sandbox, rootUnit)
	}
	// Tenants' worker sockets live under /tmp: a private /tmp would hide
	// them from the edge. Nothing stricter than read-only /etc.
	for _, no := range []string{"PrivateTmp", "ProtectSystem=strict", "ProtectHome=yes"} {
		if strings.Contains(rootUnit, no) {
			t.Errorf("root unit carries %s:\n%s", no, rootUnit)
		}
	}
	// A user unit stays unconfined: NoNewPrivileges would void the
	// setcap'd cap_net_bind_service its binary binds 80 and 443 with.
	for _, no := range []string{"NoNewPrivileges", "ProtectSystem", "ProtectHome", "ReadWritePaths", "CapabilityBoundingSet", "PrivateTmp"} {
		if strings.Contains(got, no) {
			t.Errorf("user unit carries %s:\n%s", no, got)
		}
	}
	if !strings.Contains(got, "StandardError=append:/home/ann/.local/state/janus/log/supervisor.log\n\n[Install]\n") {
		t.Errorf("user unit layout:\n%s", got)
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

// status tells a crash loop from a clean stop by the unit's last exit.
func TestParseSystemdLastExit(t *testing.T) {
	for out, want := range map[string]struct {
		code  int
		known bool
	}{
		"Result=exit-code\nExecMainStatus=1\nExecMainExitTimestampMonotonic=123\n": {1, true},
		"Result=success\nExecMainStatus=0\nExecMainExitTimestampMonotonic=123\n":   {0, true},
		"Result=signal\nExecMainStatus=0\nExecMainExitTimestampMonotonic=123\n":    {-1, true},
		"Result=success\nExecMainStatus=0\nExecMainExitTimestampMonotonic=0\n":     {0, false},
	} {
		code, known := parseSystemdLastExit([]byte(out))
		if code != want.code || known != want.known {
			t.Errorf("%q: %d %v, want %d %v", out, code, known, want.code, want.known)
		}
	}
}

// The unit reports the executable its ExecStart names, however the unit
// writer had to quote it, and restart compares that with the installed one.
func TestParseSystemdExe(t *testing.T) {
	p := servicePathsFor(false, "/home/ann", func(string) string { return "" })
	for _, exe := range []string{"/home/ann/.local/bin/janus", `/home/ann/my "bin"/janus`, "/opt/with space/janus"} {
		if got := parseSystemdExe([]byte(systemdUnitFile(p, exe))); got != exe {
			t.Errorf("parseSystemdExe = %q, want %q", got, exe)
		}
	}
	if parseSystemdExe([]byte("[Service]\nType=simple\n")) != "" {
		t.Error("parseSystemdExe invented an executable")
	}
}
