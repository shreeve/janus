package main

import (
	"net/netip"
	"strings"
	"testing"
)

// The matrix: macOS needs pf for every mode but wan (its socket is the
// wildcard); nowhere else needs a rule, because the bind is exact and lan's
// address is private.
func TestNeedsFirewallMatrix(t *testing.T) {
	cases := []struct {
		scope Scope
		goos  string
		want  bool
	}{
		{ScopeWAN, "darwin", false}, {ScopeWAN, "linux", false}, {ScopeWAN, "windows", false},
		{ScopeLAN, "darwin", true}, {ScopeLAN, "linux", false}, {ScopeLAN, "windows", false},
		{ScopeLocalhost, "darwin", true}, {ScopeLocalhost, "linux", false}, {ScopeLocalhost, "windows", false},
	}
	for _, c := range cases {
		if got := NeedsFirewall(c.scope, c.goos); got != c.want {
			t.Errorf("NeedsFirewall(%s, %s) = %v, want %v", c.scope, c.goos, got, c.want)
		}
	}
}

var testOnLink = netip.MustParsePrefix("10.0.0.0/24")

func TestPfAnchorLocalhost(t *testing.T) {
	got, err := PfAnchor(ScopeLocalhost, testOnLink)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.0/8", "{ ::1 }", "block in quick proto tcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("localhost anchor missing %q:\n%s", want, got)
		}
	}
	// localhost must not admit the on-link subnet, even though it was offered
	if strings.Contains(got, "10.0.0.0/24") {
		t.Error("localhost anchor must not admit the on-link subnet")
	}
}

func TestPfAnchorLAN(t *testing.T) {
	got, err := PfAnchor(ScopeLAN, testOnLink)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.0/8 10.0.0.0/24", "{ ::1 }", "block in quick proto tcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("lan anchor missing %q:\n%s", want, got)
		}
	}
	// IPv6 passes from the loopback only: lan is IPv4.
	if strings.Contains(got, "fe80") || strings.Contains(got, "2601") {
		t.Errorf("lan anchor admits IPv6 beyond ::1:\n%s", got)
	}
	// allows must precede the block: first-match (quick) semantics
	if strings.Index(got, "pass in quick") > strings.Index(got, "block in quick") {
		t.Error("pass rules must come before the block rule")
	}
	// The prefix is written masked, whatever was stored.
	if got2, _ := PfAnchor(ScopeLAN, netip.MustParsePrefix("10.0.0.211/24")); got2 != got {
		t.Error("an unmasked prefix produced a different anchor")
	}
	if _, err := PfAnchor(ScopeLAN, netip.Prefix{}); err == nil {
		t.Error("lan without an on-link prefix must fail")
	}
}

func TestPfAnchorWANIsEmpty(t *testing.T) {
	if got, _ := PfAnchor(ScopeWAN, testOnLink); got != "" {
		t.Errorf("wan needs no pf anchor, got:\n%s", got)
	}
}

func TestWindowsFirewallRule(t *testing.T) {
	lan := WindowsFirewallRule(ScopeLAN)
	for _, want := range []string{"LocalSubnet", "Private,Domain", "80,443"} {
		if !strings.Contains(lan, want) {
			t.Errorf("windows lan rule missing %q: %s", want, lan)
		}
	}
	if strings.Contains(lan, "Public") {
		t.Error("windows lan rule must not open the Public profile")
	}
	if !strings.Contains(WindowsFirewallRule(ScopeWAN), "Profile Any") {
		t.Error("windows wan rule must allow all profiles")
	}
	if WindowsFirewallRule(ScopeLocalhost) != "" {
		t.Error("windows localhost needs no firewall rule (loopback is exempt)")
	}
}
