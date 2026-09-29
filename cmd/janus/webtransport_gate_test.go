package main

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestWebtransportAnchorReady(t *testing.T) {
	st := scopeState{Scope: ScopeLAN, Interface: "en0", LANV4: "10.0.0.27", OnLinkV4: "10.0.0.0/24"}
	want, err := PfAnchor(st.Scope, st.onlink())
	if err != nil {
		t.Fatal(err)
	}
	withBlock := func(servicePaths) ([]byte, error) { return []byte(`{"apps":{"janus":{"webtransport":{}}}}`), nil }
	without := func(servicePaths) ([]byte, error) { return []byte(`{"apps":{"janus":{}}}`), nil }
	files := map[string]string{}
	ops := fwOps{goos: "darwin",
		read: func(path string) ([]byte, error) {
			if s, ok := files[path]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		},
		run: func(string, ...string) ([]byte, error) { return nil, nil },
	}
	wired := func(anchor string) {
		files[pfAnchorPath] = anchor
		files[pfConfPath] = pfConfAnchor + "\n" + pfConfLoadLine + "\n"
		files[pfDaemonPlist] = pfDaemonPlistBody()
	}
	// No block: nothing to check, whatever the anchor says.
	if err := webtransportAnchorReady(ops, servicePaths{}, st, without); err != nil {
		t.Fatalf("no block: %v", err)
	}
	// Block, anchor absent: refused, naming the verb.
	err = webtransportAnchorReady(ops, servicePaths{}, st, withBlock)
	if err == nil || !strings.Contains(err.Error(), "janus mode lan") || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("absent anchor: %v", err)
	}
	// Block, the pre-relay TCP-only anchor: refused as a mismatch.
	wired(strings.ReplaceAll(want, "proto { tcp udp }", "proto tcp"))
	err = webtransportAnchorReady(ops, servicePaths{}, st, withBlock)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("stale anchor: %v", err)
	}
	// Block, current anchor: ready.
	wired(want)
	if err := webtransportAnchorReady(ops, servicePaths{}, st, withBlock); err != nil {
		t.Fatalf("current anchor: %v", err)
	}
	// wan needs no anchor; Linux has none.
	files[pfAnchorPath] = "stale"
	if err := webtransportAnchorReady(ops, servicePaths{}, scopeState{Scope: ScopeWAN}, withBlock); err != nil {
		t.Fatalf("wan: %v", err)
	}
	linux := ops
	linux.goos = "linux"
	if err := webtransportAnchorReady(linux, servicePaths{}, st, withBlock); err != nil {
		t.Fatalf("linux: %v", err)
	}
	// An unadaptable Caddyfile is the edge's own error to report.
	failing := func(servicePaths) ([]byte, error) { return nil, errors.New("bad Caddyfile") }
	if err := webtransportAnchorReady(ops, servicePaths{}, st, failing); err != nil {
		t.Fatalf("unadaptable: %v", err)
	}
}
