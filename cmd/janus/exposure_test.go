package main

import (
	"net/netip"
	"testing"
)

func TestParseScope(t *testing.T) {
	for _, in := range []string{"localhost", "LAN", " wan ", "Localhost"} {
		if _, err := ParseScope(in); err != nil {
			t.Errorf("ParseScope(%q) unexpected error: %v", in, err)
		}
	}
	for _, in := range []string{"", "local", "public", "loopback", "internet"} {
		if _, err := ParseScope(in); err == nil {
			t.Errorf("ParseScope(%q) should have failed", in)
		}
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func addrSet(specs []ListenerSpec) map[string]Role {
	m := map[string]Role{}
	for _, s := range specs {
		m[s.Addr.String()] = s.Role
	}
	return m
}

func TestPlanLocalhost(t *testing.T) {
	specs, err := PlanListeners(ScopeLocalhost, LANAddrs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 {
		t.Fatalf("localhost: want 4 listeners, got %d", len(specs))
	}
	got := addrSet(specs)
	for _, want := range []string{"127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::1]:443"} {
		if _, ok := got[want]; !ok {
			t.Errorf("localhost: missing %s", want)
		}
	}
	if got["127.0.0.1:443"] != RoleHTTPS || got["127.0.0.1:80"] != RoleHTTP {
		t.Errorf("localhost: role by port wrong: %+v", got)
	}
}

func TestPlanLAN(t *testing.T) {
	lan := LANAddrs{V4: mustAddr("10.0.0.211"), V6: mustAddr("2601:680:8000:2330::906e")}
	specs, err := PlanListeners(ScopeLAN, lan)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 8 {
		t.Fatalf("lan v4+v6: want 8 listeners, got %d", len(specs))
	}
	got := addrSet(specs)
	for _, want := range []string{"10.0.0.211:443", "[2601:680:8000:2330::906e]:443"} {
		if _, ok := got[want]; !ok {
			t.Errorf("lan: missing %s", want)
		}
	}
}

func TestPlanLANv4Only(t *testing.T) {
	specs, err := PlanListeners(ScopeLAN, LANAddrs{V4: mustAddr("192.168.1.42")})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 6 { // loopback v4+v6 (4) + one lan v4 (2)
		t.Fatalf("lan v4 only: want 6 listeners, got %d", len(specs))
	}
}

func TestPlanLANRequiresAddress(t *testing.T) {
	if _, err := PlanListeners(ScopeLAN, LANAddrs{}); err == nil {
		t.Fatal("lan with no interface address must fail closed, not decay to localhost")
	}
}

func TestPlanLANRejectsBadAddrs(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "::1", "fe80::1", "0.0.0.0"} {
		if _, err := PlanListeners(ScopeLAN, LANAddrs{V4: mustAddr("10.0.0.5"), V6: mustAddr(bad)}); err == nil {
			// bad supplied via V6 slot for link-local/loopback cases; v4 ones via V4 below
			t.Errorf("lan accepted disallowed address %s", bad)
		}
	}
	if _, err := PlanListeners(ScopeLAN, LANAddrs{V4: mustAddr("127.0.0.1")}); err == nil {
		t.Error("lan accepted loopback v4 as an interface address")
	}
}

func TestPlanWAN(t *testing.T) {
	specs, err := PlanListeners(ScopeWAN, LANAddrs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 {
		t.Fatalf("wan: want 4 listeners, got %d", len(specs))
	}
	got := addrSet(specs)
	for _, want := range []string{"0.0.0.0:80", "0.0.0.0:443", "[::]:80", "[::]:443"} {
		if _, ok := got[want]; !ok {
			t.Errorf("wan: missing %s", want)
		}
	}
}

func TestNormalizeMapped(t *testing.T) {
	mapped := netip.MustParseAddr("::ffff:127.0.0.1")
	if got := normalizeAddr(mapped); got != mustAddr("127.0.0.1") {
		t.Errorf("normalizeAddr(::ffff:127.0.0.1) = %s, want 127.0.0.1", got)
	}
}

func eqStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBindPlan(t *testing.T) {
	lan := LANAddrs{V4: mustAddr("10.0.0.211"), V6: mustAddr("2601:680:8000:2330::906e")}
	wild := []string{"0.0.0.0", "::"}

	// macOS: every mode binds the wildcard; pf enforces the scope.
	for _, sc := range []Scope{ScopeLocalhost, ScopeLAN, ScopeWAN} {
		got, err := BindPlan(sc, "darwin", lan)
		if err != nil {
			t.Fatal(err)
		}
		if !eqStrs(got, wild) {
			t.Errorf("darwin %s: got %v, want wildcard", sc, got)
		}
	}
	// Linux/Windows bind the exact addresses.
	if got, _ := BindPlan(ScopeLocalhost, "linux", LANAddrs{}); !eqStrs(got, []string{"127.0.0.1", "::1"}) {
		t.Errorf("linux localhost: %v", got)
	}
	if got, _ := BindPlan(ScopeLAN, "linux", lan); !eqStrs(got, []string{"127.0.0.1", "::1", "10.0.0.211", "2601:680:8000:2330::906e"}) {
		t.Errorf("linux lan: %v", got)
	}
	if got, _ := BindPlan(ScopeWAN, "windows", LANAddrs{}); !eqStrs(got, wild) {
		t.Errorf("windows wan: %v", got)
	}
	// lan requires an interface address on every OS — on macOS the pf on-link
	// rule needs it even though the bind is wildcard.
	if _, err := BindPlan(ScopeLAN, "darwin", LANAddrs{}); err == nil {
		t.Error("lan with no interface address must fail, even on darwin")
	}
}

func aps(ss ...string) []netip.AddrPort {
	out := make([]netip.AddrPort, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddrPort(s)
	}
	return out
}

func TestVerifyBoundExact(t *testing.T) {
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::1]:443")
	if err := VerifyBound(ScopeLocalhost, LANAddrs{}, bound); err != nil {
		t.Errorf("exact localhost bind should verify: %v", err)
	}
}

func TestVerifyBoundRejectsWildcardInLocalhost(t *testing.T) {
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "0.0.0.0:443")
	if err := VerifyBound(ScopeLocalhost, LANAddrs{}, bound); err == nil {
		t.Error("a wildcard listener in localhost scope must fail verification")
	}
}

func TestVerifyBoundRejectsMappedWildcardLeak(t *testing.T) {
	// A v6 wildcard answering v4 is the canonical dual-stack leak; even its
	// mapped-unspecified form must be rejected outside wan.
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::]:443")
	if err := VerifyBound(ScopeLocalhost, LANAddrs{}, bound); err == nil {
		t.Error("a v6 wildcard in localhost scope must fail verification")
	}
}

func TestVerifyBoundMissingRequired(t *testing.T) {
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80") // missing [::1]:443
	if err := VerifyBound(ScopeLocalhost, LANAddrs{}, bound); err == nil {
		t.Error("a missing required listener must fail verification")
	}
}

func TestVerifyBoundSurplus(t *testing.T) {
	lan := LANAddrs{V4: mustAddr("10.0.0.211")}
	// surplus: an in-set address bound on an unexpected port (8443)
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::1]:443",
		"10.0.0.211:80", "10.0.0.211:443", "10.0.0.211:8443")
	if err := VerifyBound(ScopeLAN, lan, bound); err == nil {
		t.Error("a surplus listener must fail verification")
	}
}

func TestVerifyBoundWANAcceptsWildcard(t *testing.T) {
	bound := aps("0.0.0.0:80", "0.0.0.0:443", "[::]:80", "[::]:443")
	if err := VerifyBound(ScopeWAN, LANAddrs{}, bound); err != nil {
		t.Errorf("wan wildcard bind should verify: %v", err)
	}
}

func TestClassifyReach(t *testing.T) {
	cases := []struct {
		scope       Scope
		addr        string
		lanFirewall bool
		want        Reach
	}{
		{ScopeLocalhost, "127.0.0.1", false, ReachLoopback},
		{ScopeLocalhost, "::1", false, ReachLoopback},
		{ScopeLAN, "10.0.0.211", true, ReachRFC1918},
		{ScopeLAN, "2601:680:8000:2330::906e", false, ReachRouter},   // global v6, no host rule
		{ScopeLAN, "2601:680:8000:2330::906e", true, ReachFirewall},  // global v6, on-link rule
		{ScopeLAN, "fd42:a6fc:fdc:d768::1", false, ReachULA},
		{ScopeWAN, "0.0.0.0", false, ReachWildcard},
		{ScopeWAN, "::", false, ReachWildcard},
	}
	for _, c := range cases {
		if got := classifyReach(c.scope, mustAddr(c.addr), c.lanFirewall); got != c.want {
			t.Errorf("classifyReach(%s, %s, fw=%v) = %q, want %q", c.scope, c.addr, c.lanFirewall, got, c.want)
		}
	}
}
