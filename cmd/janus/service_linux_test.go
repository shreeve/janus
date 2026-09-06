package main

import (
	"strings"
	"testing"
)

func TestSystemdUnit(t *testing.T) {
	p := servicePathsFor(false, "/home/ann", func(string) string { return "" })
	got := systemdUnitFile(p, "/home/ann/.local/bin/janus")
	for _, want := range []string{
		`ExecStart="/home/ann/.local/bin/janus" run --config "/home/ann/.config/janus/Caddyfile" --adapter caddyfile`,
		"Restart=on-failure",
		"RestartSec=10",
		"WantedBy=default.target",
		"StandardOutput=append:/home/ann/.local/state/janus/log/supervisor.log",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit lacks %q:\n%s", want, got)
		}
	}
	item := newServiceItem(p).(*systemdItem)
	if item.unit != "/home/ann/.config/systemd/user/janus.service" || !item.user {
		t.Errorf("user item: %+v", item)
	}
	rp := servicePathsFor(true, "/root", func(string) string { return "" })
	if !strings.Contains(systemdUnitFile(rp, "/usr/local/bin/janus"), "WantedBy=multi-user.target") {
		t.Error("root unit should want multi-user.target")
	}
	root := newServiceItem(rp).(*systemdItem)
	if root.unit != "/etc/systemd/system/janus.service" || root.user {
		t.Errorf("root item: %+v", root)
	}
}
