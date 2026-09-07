package main

import (
	"net/netip"
	"strings"
	"testing"
)

func TestNeedsFirewallMatrix(t *testing.T) {
	cases := []struct {
		scope Scope
		goos  string
		want  bool
	}{
		// wan: never
		{ScopeWAN, "darwin", false}, {ScopeWAN, "linux", false}, {ScopeWAN, "windows", false},
		// lan: everywhere (the IPv6 address is globally routable)
		{ScopeLAN, "darwin", true}, {ScopeLAN, "linux", true}, {ScopeLAN, "windows", true},
		// localhost: macOS only (wildcard socket + pf stands in for an exact bind)
		{ScopeLocalhost, "darwin", true}, {ScopeLocalhost, "linux", false}, {ScopeLocalhost, "windows", false},
	}
	for _, c := range cases {
		if got := NeedsFirewall(c.scope, c.goos); got != c.want {
			t.Errorf("NeedsFirewall(%s, %s) = %v, want %v", c.scope, c.goos, got, c.want)
		}
	}
}

var testOnLink = OnLink{
	V4: netip.MustParsePrefix("10.0.0.0/24"),
	V6: netip.MustParsePrefix("2601:680:8000:2330::/64"),
}

func TestPfAnchorLocalhost(t *testing.T) {
	got, err := PfAnchor(ScopeLocalhost, testOnLink)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.0/8", "::1", "block in quick proto tcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("localhost anchor missing %q:\n%s", want, got)
		}
	}
	// localhost must not admit the on-link subnet, even though it was offered
	for _, forbid := range []string{"10.0.0.0/24", "2601:680:8000:2330::/64", "fe80::/10"} {
		if strings.Contains(got, forbid) {
			t.Errorf("localhost anchor must not admit %q", forbid)
		}
	}
}

func TestPfAnchorLAN(t *testing.T) {
	got, err := PfAnchor(ScopeLAN, testOnLink)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.0/8", "10.0.0.0/24", "::1", "fe80::/10", "2601:680:8000:2330::/64", "block in quick proto tcp"} {
		if !strings.Contains(got, want) {
			t.Errorf("lan anchor missing %q:\n%s", want, got)
		}
	}
	// allows must precede the block: first-match (quick) semantics
	if strings.Index(got, "pass in quick") > strings.Index(got, "block in quick") {
		t.Error("pass rules must come before the block rule")
	}
}

func TestPfAnchorWANIsEmpty(t *testing.T) {
	if got, _ := PfAnchor(ScopeWAN, testOnLink); got != "" {
		t.Errorf("wan needs no pf anchor, got:\n%s", got)
	}
}

func TestNftRules(t *testing.T) {
	got, err := NftRules(ScopeLAN, testOnLink)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2601:680:8000:2330::/64", "fe80::/10", "::1", "accept", "meta nfproto ipv6 drop"} {
		if !strings.Contains(got, want) {
			t.Errorf("lan nft missing %q:\n%s", want, got)
		}
	}
	// the RFC1918 v4 address is a bound exact address; no v4 rule is emitted
	if strings.Contains(got, "10.0.0.0/24") {
		t.Error("lan nft must not filter IPv4 — the RFC1918 bind needs no rule")
	}
	// localhost and wan need no nft on Linux
	for _, sc := range []Scope{ScopeLocalhost, ScopeWAN} {
		if r, _ := NftRules(sc, testOnLink); r != "" {
			t.Errorf("%s needs no nft rules on Linux, got:\n%s", sc, r)
		}
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
