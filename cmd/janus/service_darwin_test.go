package main

import (
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	p := servicePathsFor(false, "/Users/ann", func(string) string { return "" })
	got := launchdPlist(p, "/Users/ann/.local/bin/janus")
	for _, want := range []string{
		"<string>janus.edge</string>",
		"<string>/Users/ann/.local/bin/janus</string>",
		"<string>run</string>",
		"<string>/Users/ann/.config/janus/Caddyfile</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
		"<integer>10</integer>",
		"<string>/Users/ann/.local/state/janus/log/supervisor.log</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist lacks %q:\n%s", want, got)
		}
	}
	item := newServiceItem(p).(*launchdItem)
	if item.plist != "/Users/ann/Library/LaunchAgents/janus.edge.plist" || !strings.HasPrefix(item.domain, "gui/") {
		t.Errorf("user item: %+v", item)
	}
	root := newServiceItem(servicePathsFor(true, "/var/root", func(string) string { return "" })).(*launchdItem)
	if root.plist != "/Library/LaunchDaemons/janus.edge.plist" || root.domain != "system" {
		t.Errorf("root item: %+v", root)
	}
}
