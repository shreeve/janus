package main

import (
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	p := servicePathsFor(false, "/Users/ann", func(string) string { return "" })
	got := launchdPlist("janus.edge", p, "/Users/ann/.local/bin/janus")
	for _, want := range []string{
		"<string>janus.edge</string>",
		"<string>/Users/ann/.local/bin/janus</string>",
		"<string>run</string>",
		"<string>/Users/ann/.config/janus/Caddyfile</string>",
		"<key>HOME</key>\n\t\t<string>/Users/ann</string>",
		"<key>PATH</key>\n\t\t<string>/Users/ann/.local/bin:/usr/bin:/bin</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<integer>10</integer>",
		// A stop that Caddy cannot finish is ended here, not waited on.
		"<key>ExitTimeOut</key>\n\t<integer>15</integer>",
		// Normal scheduling priority: the browser is waiting on every
		// request through the edge, and launchd's Background band would
		// starve it under load.
		"<key>ProcessType</key>\n\t<string>Standard</string>",
		"<string>/Users/ann/.local/state/janus/log/supervisor.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	// Crash-only: never KeepAlive true, which would revive a clean stop.
	if strings.Contains(got, "<key>KeepAlive</key>\n\t<true/>") {
		t.Error("KeepAlive is unconditional")
	}
	item := newServiceItem(p).(*launchdItem)
	if item.plist != "/Users/ann/Library/LaunchAgents/janus.edge.plist" || !strings.HasPrefix(item.domain, "gui/") {
		t.Errorf("user item: %+v", item)
	}
	root := newServiceItem(servicePathsFor(true, "/var/root", func(string) string { return "" })).(*launchdItem)
	if root.plist != "/Library/LaunchDaemons/janus.edge.plist" || root.domain != "system" {
		t.Errorf("root item: %+v", root)
	}
	t.Setenv("JANUS_SERVICE_LABEL", "janus.test7")
	alt := newServiceItem(p).(*launchdItem)
	if alt.label != "janus.test7" || alt.plist != "/Users/ann/Library/LaunchAgents/janus.test7.plist" || alt.target() != item.domain+"/janus.test7" {
		t.Errorf("label override: %+v", alt)
	}
}

func TestParseLaunchdPID(t *testing.T) {
	running := []byte("gui/501/janus.edge = {\n\tactive count = 1\n\tpath = /Users/ann/Library/LaunchAgents/janus.edge.plist\n\tstate = running\n\n\tprogram = /Users/ann/.local/bin/janus\n\tpid = 31291\n\tsub-process count = 0\n}")
	if got := parseLaunchdPID(running); got != 31291 {
		t.Errorf("running: pid %d", got)
	}
	stopped := []byte("gui/501/janus.edge = {\n\tstate = not running\n\tlast exit code = 0\n}")
	if got := parseLaunchdPID(stopped); got != 0 {
		t.Errorf("stopped: pid %d", got)
	}
	scheduled := []byte("gui/501/janus.edge = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}")
	if got := parseLaunchdPID(scheduled); got != 0 {
		t.Errorf("scheduled: pid %d", got)
	}
}
