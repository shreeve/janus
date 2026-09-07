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
	specs, err := PlanListeners(ScopeLocalhost, netip.Addr{})
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
	specs, err := PlanListeners(ScopeLAN, mustAddr("10.0.0.211"))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 6 { // loopback v4+v6 (4) + the lan v4 (2)
		t.Fatalf("lan: want 6 listeners, got %d", len(specs))
	}
	got := addrSet(specs)
	for _, want := range []string{"10.0.0.211:80", "10.0.0.211:443", "[::1]:443"} {
		if _, ok := got[want]; !ok {
			t.Errorf("lan: missing %s", want)
		}
	}
}

func TestPlanLANRequiresAddress(t *testing.T) {
	if _, err := PlanListeners(ScopeLAN, netip.Addr{}); err == nil {
		t.Fatal("lan with no interface address must fail closed, not decay to localhost")
	}
}

// Only a private IPv4 address may be the lan address: loopback, wildcard,
// link-local, any IPv6 (globally routable or not), and public IPv4 are all
// refused by the backstop.
func TestPlanLANRejectsBadAddrs(t *testing.T) {
	for _, bad := range []string{"127.0.0.1", "0.0.0.0", "169.254.3.4", "224.0.0.1", "::1", "fe80::1", "2601:680:8000:2330::906e", "fd42:a6fc:fdc:d768::1", "::ffff:8.8.8.8", "8.8.8.8", "34.22.36.78"} {
		if _, err := PlanListeners(ScopeLAN, mustAddr(bad)); err == nil {
			t.Errorf("lan accepted disallowed address %s", bad)
		}
	}
	// A mapped private address is its plain self.
	if specs, err := PlanListeners(ScopeLAN, mustAddr("::ffff:192.168.1.9")); err != nil || addrSet(specs)["192.168.1.9:443"] != RoleHTTPS {
		t.Errorf("mapped private address: %v %v", err, specs)
	}
}

func TestPlanWAN(t *testing.T) {
	specs, err := PlanListeners(ScopeWAN, netip.Addr{})
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
	lan := mustAddr("10.0.0.211")
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
	if got, _ := BindPlan(ScopeLocalhost, "linux", netip.Addr{}); !eqStrs(got, []string{"127.0.0.1", "::1"}) {
		t.Errorf("linux localhost: %v", got)
	}
	if got, _ := BindPlan(ScopeLAN, "linux", lan); !eqStrs(got, []string{"127.0.0.1", "::1", "10.0.0.211"}) {
		t.Errorf("linux lan: %v", got)
	}
	if got, _ := BindPlan(ScopeWAN, "windows", netip.Addr{}); !eqStrs(got, wild) {
		t.Errorf("windows wan: %v", got)
	}
	// lan requires an interface address on every OS — on macOS the pf on-link
	// rule needs it even though the bind is wildcard.
	if _, err := BindPlan(ScopeLAN, "darwin", netip.Addr{}); err == nil {
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
	if err := VerifyBound(ScopeLocalhost, netip.Addr{}, bound); err != nil {
		t.Errorf("exact localhost bind should verify: %v", err)
	}
}

func TestVerifyBoundRejectsWildcardInLocalhost(t *testing.T) {
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "0.0.0.0:443")
	if err := VerifyBound(ScopeLocalhost, netip.Addr{}, bound); err == nil {
		t.Error("a wildcard listener in localhost scope must fail verification")
	}
}

func TestVerifyBoundRejectsMappedWildcardLeak(t *testing.T) {
	// A v6 wildcard answering v4 is the canonical dual-stack leak; even its
	// mapped-unspecified form must be rejected outside wan.
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::]:443")
	if err := VerifyBound(ScopeLocalhost, netip.Addr{}, bound); err == nil {
		t.Error("a v6 wildcard in localhost scope must fail verification")
	}
}

// Fewer listeners than the plan is less exposure, never a violation: an
// empty sites directory binds nothing at all.
func TestVerifyBoundFewerIsFine(t *testing.T) {
	bound := aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80") // no [::1]:443
	if err := VerifyBound(ScopeLocalhost, netip.Addr{}, bound); err != nil {
		t.Errorf("a subset of the plan must verify: %v", err)
	}
	if err := VerifyBound(ScopeLAN, mustAddr("10.0.0.211"), nil); err != nil {
		t.Errorf("nothing bound must verify: %v", err)
	}
}

func TestVerifyBoundSurplus(t *testing.T) {
	lan := mustAddr("10.0.0.211")
	// surplus: an in-set address bound on an unexpected port (8443), and a
	// lan edge that also bound the interface's IPv6 address
	for _, bound := range [][]netip.AddrPort{
		aps("127.0.0.1:80", "127.0.0.1:443", "[::1]:80", "[::1]:443", "10.0.0.211:80", "10.0.0.211:443", "10.0.0.211:8443"),
		aps("10.0.0.211:443", "[2601:680:8000:2330::906e]:443"),
	} {
		if err := VerifyBound(ScopeLAN, lan, bound); err == nil {
			t.Errorf("a surplus listener must fail verification: %v", bound)
		}
	}
}

func TestVerifyBoundWANAcceptsWildcard(t *testing.T) {
	bound := aps("0.0.0.0:80", "0.0.0.0:443", "[::]:80", "[::]:443")
	if err := VerifyBound(ScopeWAN, netip.Addr{}, bound); err != nil {
		t.Errorf("wan wildcard bind should verify: %v", err)
	}
}

func TestClassifyReach(t *testing.T) {
	cases := map[string]Reach{
		"127.0.0.1":    ReachLoopback,
		"::1":          ReachLoopback,
		"10.0.0.211":   ReachRFC1918,
		"192.168.1.42": ReachRFC1918,
		"0.0.0.0":      ReachWildcard,
		"::":           ReachWildcard,
	}
	for addr, want := range cases {
		if got := classifyReach(mustAddr(addr)); got != want {
			t.Errorf("classifyReach(%s) = %q, want %q", addr, got, want)
		}
	}
}
