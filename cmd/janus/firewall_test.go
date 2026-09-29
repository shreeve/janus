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
	for _, want := range []string{"from { 127.0.0.0/8 } to any port 80", "proto { tcp udp } from { 127.0.0.0/8 } to any port 443", "{ ::1 }", "block in quick proto tcp from any to any port 80", "block in quick proto { tcp udp } from any to any port 443"} {
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
	for _, want := range []string{"proto tcp from { 127.0.0.0/8 10.0.0.0/24 } to any port 80", "proto { tcp udp } from { 127.0.0.0/8 10.0.0.0/24 } to any port 443", "{ ::1 }", "block in quick proto tcp", "block in quick proto { tcp udp }"} {
		if !strings.Contains(got, want) {
			t.Errorf("lan anchor missing %q:\n%s", want, got)
		}
	}
	// IPv6 passes from the loopback only: lan is IPv4.
	if strings.Contains(got, "fe80") || strings.Contains(got, "2601") {
		t.Errorf("lan anchor admits IPv6 beyond ::1:\n%s", got)
	}
	// allows must precede the block: first-match (quick) semantics
	if strings.LastIndex(got, "pass in quick") > strings.Index(got, "block in quick") {
		t.Error("pass rules must come before the block rules")
	}
	// The prefix is written masked, whatever was stored.
	if got2, _ := PfAnchor(ScopeLAN, netip.MustParsePrefix("10.0.0.211/24")); got2 != got {
		t.Error("an unmasked prefix produced a different anchor")
	}
	if _, err := PfAnchor(ScopeLAN, netip.Prefix{}); err == nil {
		t.Error("lan without an on-link prefix must fail")
	}
}

// UDP is the webtransport relay's: it passes on 443 alone, beside TCP and
// from the same sources, and is blocked there for everyone else. Port 80
// stays TCP only, pass and block.
func TestPfAnchorUDPOn443Only(t *testing.T) {
	for _, scope := range []Scope{ScopeLocalhost, ScopeLAN} {
		got, err := PfAnchor(scope, testOnLink)
		if err != nil {
			t.Fatal(err)
		}
		var udp, tcpOnly int
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			switch {
			case strings.HasPrefix(line, "#"):
			case strings.Contains(line, "udp"):
				udp++
				if !strings.Contains(line, "proto { tcp udp }") || !strings.Contains(line, " port 443") || strings.Contains(line, "80") {
					t.Errorf("%s: udp outside 443, or without tcp beside it: %s", scope, line)
				}
			default:
				tcpOnly++
				if !strings.Contains(line, "proto tcp ") || !strings.Contains(line, " port 80") {
					t.Errorf("%s: a line without udp must be tcp on 80: %s", scope, line)
				}
			}
		}
		// inet pass, inet6 pass, block: for each port.
		if udp != 3 || tcpOnly != 3 {
			t.Errorf("%s: %d udp-bearing lines and %d tcp-only lines, want 3 and 3:\n%s", scope, udp, tcpOnly, got)
		}
	}
}

func TestPfAnchorWANIsEmpty(t *testing.T) {
	if got, _ := PfAnchor(ScopeWAN, testOnLink); got != "" {
		t.Errorf("wan needs no pf anchor, got:\n%s", got)
	}
}

// Two rules per opening mode, TCP 80,443 and the webtransport relay's UDP
// 443, under the same profiles and with distinct names that the check and
// removal paths can look up; none for localhost.
func TestWindowsFirewallRules(t *testing.T) {
	lan := WindowsFirewallRules(ScopeLAN)
	if len(lan) != 2 {
		t.Fatalf("windows lan rules: %q", lan)
	}
	for i, rule := range lan {
		for _, want := range []string{"LocalSubnet", "Private,Domain", "-Direction Inbound -Action Allow", `-DisplayName "` + windowsRuleNames(ScopeLAN)[i] + `"`} {
			if !strings.Contains(rule, want) {
				t.Errorf("windows lan rule missing %q: %s", want, rule)
			}
		}
		if strings.Contains(rule, "Public") {
			t.Errorf("windows lan rule must not open the Public profile: %s", rule)
		}
	}
	if !strings.Contains(lan[0], "-Protocol TCP -LocalPort 80,443") || !strings.Contains(lan[1], "-Protocol UDP -LocalPort 443") {
		t.Errorf("windows lan rules: tcp 80,443 then udp 443: %q", lan)
	}
	if names := windowsRuleNames(ScopeLAN); len(names) != 2 || names[0] == names[1] || !strings.Contains(names[1], "webtransport") {
		t.Errorf("windows lan rule names: %q", names)
	}
	wan := WindowsFirewallRules(ScopeWAN)
	if len(wan) != 2 || !strings.Contains(wan[0], "Profile Any") || !strings.Contains(wan[1], "-Protocol UDP -LocalPort 443 -Profile Any") {
		t.Errorf("windows wan rules must allow all profiles for both: %q", wan)
	}
	if WindowsFirewallRules(ScopeLocalhost) != nil || windowsRuleNames(ScopeLocalhost) != nil {
		t.Error("windows localhost needs no firewall rule (loopback is exempt)")
	}
}
