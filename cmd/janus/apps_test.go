package main

import (
	"errors"
	"strings"
	"testing"
)

const appsFixture = `[{"id":"a1","name":"shop","hosts":["shop.via.rip","shop.local"],"upstreams":[{"path":"/tmp/w1.sock"},{"path":"/tmp/w2.sock"}],"lease":"heartbeat"},` +
	`{"id":"b2","name":"browse","hosts":["browse-1f2e.localhost"],"upstreams":[],"files":{"roots":[{"path":"/Users/ann/docs","browse":true}]},"lease":"process"},` +
	`{"id":"c3","name":"medlabs","hosts":[],"upstreams":[{"path":"/tmp/bell.sock","doorbell":true}],"site":{"host":"{site}.medlabs.health","dir":"/srv/medlabs/sites"},"lease":"heartbeat"}]`

// apps lists the registry over the control socket, one line per app,
// saying what serves each; --json is the API's own answer; no edge is
// exit 3.
func TestAppsVerb(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	_, err := run(t, "apps")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("no edge: %v", err)
	}
	controlServer(t, p, appsFixture)
	out, err := run(t, "apps")
	if err != nil {
		t.Fatalf("apps: %v\n%s", err, out)
	}
	for _, want := range []string{"3 apps registered (control unix " + p.sock + ")", "NAME", "shop", "shop.via.rip shop.local", "2 workers", "heartbeat", "browse /Users/ann/docs", "process", "doorbell /tmp/bell.sock, sites in /srv/medlabs/sites", "c3"} {
		if !strings.Contains(out, want) {
			t.Errorf("apps lacks %q:\n%s", want, out)
		}
	}
	if out, err := run(t, "apps", "--json"); err != nil || strings.TrimSpace(out) != appsFixture {
		t.Errorf("apps --json: %v\n%s", err, out)
	}
}

func TestAppsVerbEmpty(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	controlServer(t, p, `[]`)
	if out, err := run(t, "apps"); err != nil || !strings.Contains(out, "no apps registered") {
		t.Errorf("empty registry: %v\n%s", err, out)
	}
}
