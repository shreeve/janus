package main

// Exposure modes: the portable core of the localhost|lan|wan contract. A
// mode is a list of exact addresses to listen on. This file is the shared
// spine every platform builds on — it plans the address list, decides what a
// bound listener is allowed to be, verifies what actually bound against the
// plan (fail-closed), and classifies each address so status can tell the
// truth about reachability. The state lives in scope.json (scope.go), the
// firewall in firewall.go and firewall_apply.go, the verbs in
// exposure_verbs.go; everything here is deterministic and OS-independent.
//
// Mechanism, settled per platform:
//   - Linux, Windows: bind the exact addresses directly (Linux needs
//     CAP_NET_BIND_SERVICE for the low ports; Windows needs nothing).
//   - macOS: cannot bind an exact low port unprivileged, so every mode binds
//     the wildcard and the host firewall (pf) enforces the scope. The mode is
//     therefore invisible to the Caddyfile on macOS.
//
// lan is IPv4: localhost plus one private (RFC1918) address of one interface.
// A private address is unreachable from the internet by addressing alone, so
// lan needs no firewall where the bind is exact; only macOS's wildcard socket
// needs one. IPv6 stays on the loopback (::1) and on wan (::).

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Scope is the exposure mode: which addresses the front door listens on.
type Scope string

const (
	ScopeLocalhost Scope = "localhost" // 127.0.0.1 + ::1 only — absolute, host-enforced
	ScopeLAN       Scope = "lan"       // localhost + one interface's private IPv4 address
	ScopeWAN       Scope = "wan"       // all interfaces (0.0.0.0 + ::)
)

// DefaultScope is the mode when none is named. localhost is the one mode that
// can make an absolute promise (identical on v4/v6, independent of NAT, router,
// or firewall), so it is the fail-closed default.
const DefaultScope = ScopeLocalhost

// ParseScope reads a scope name, case-insensitively. Unknown names are an
// error, never a silent fallback.
func ParseScope(s string) (Scope, error) {
	switch Scope(strings.ToLower(strings.TrimSpace(s))) {
	case ScopeLocalhost:
		return ScopeLocalhost, nil
	case ScopeLAN:
		return ScopeLAN, nil
	case ScopeWAN:
		return ScopeWAN, nil
	}
	return "", fmt.Errorf("unknown exposure mode %q: use localhost, lan, or wan", s)
}

// edgePorts are the ports every mode binds, on every allowed address.
var edgePorts = []uint16{80, 443}

// Role names which server a listener belongs to: the plain-HTTP :80 server
// (redirects, ACME HTTP-01) or the TLS-terminating :443 server.
type Role int

const (
	RoleHTTP Role = iota
	RoleHTTPS
)

func (r Role) String() string {
	switch r {
	case RoleHTTP:
		return "http"
	case RoleHTTPS:
		return "https"
	}
	return "unknown"
}

func roleForPort(port uint16) Role {
	if port == 443 {
		return RoleHTTPS
	}
	return RoleHTTP
}

// ListenerSpec is one exact address the edge must listen on.
type ListenerSpec struct {
	Role Role
	Addr netip.AddrPort
}

func loopbackAddrs() []netip.Addr {
	return []netip.Addr{
		netip.AddrFrom4([4]byte{127, 0, 0, 1}),
		netip.IPv6Loopback(),
	}
}

// PlanListeners turns a scope into the exact set of listeners to acquire. It is
// pure and deterministic — the heart of the contract. lan is the selected
// interface's private IPv4 address (zero for none). It rejects anything that
// would silently widen the scope: an unspecified address outside wan, a
// loopback, link-local, multicast, IPv6, or public address offered as the lan
// address, or a lan mode with no address at all (which must fail closed, not
// decay to localhost).
func PlanListeners(scope Scope, lan netip.Addr) ([]ListenerSpec, error) {
	var addrs []netip.Addr
	switch scope {
	case ScopeLocalhost:
		addrs = loopbackAddrs()
	case ScopeLAN:
		if !lan.IsValid() {
			return nil, fmt.Errorf("lan mode requires the selected interface's IPv4 address")
		}
		if err := validLANAddr(lan); err != nil {
			return nil, err
		}
		addrs = append(loopbackAddrs(), normalizeAddr(lan))
	case ScopeWAN:
		addrs = []netip.Addr{
			netip.IPv4Unspecified(),
			netip.IPv6Unspecified(),
		}
	default:
		return nil, fmt.Errorf("unknown exposure mode %q", scope)
	}

	specs := make([]ListenerSpec, 0, len(addrs)*len(edgePorts))
	for _, a := range addrs {
		for _, port := range edgePorts {
			specs = append(specs, ListenerSpec{
				Role: roleForPort(port),
				Addr: netip.AddrPortFrom(a, port),
			})
		}
	}
	return specs, nil
}

// validLANAddr rejects addresses that must never be the lan address: only a
// private IPv4 address is unreachable from the internet by addressing alone,
// which is what lets lan need no firewall where the bind is exact. Interface
// enumeration is expected to exclude these already; this is the fail-closed
// backstop.
func validLANAddr(a netip.Addr) error {
	n := normalizeAddr(a)
	switch {
	case !n.IsValid():
		return fmt.Errorf("lan address is not a valid IP")
	case n.Is6():
		return fmt.Errorf("lan address %s is IPv6; lan is IPv4 (a global IPv6 address is reachable from the internet)", a)
	case n.IsUnspecified():
		return fmt.Errorf("lan address %s is unspecified (wildcard) — not allowed outside wan", a)
	case n.IsLoopback():
		return fmt.Errorf("lan address %s is loopback — loopback is bound implicitly", a)
	case n.IsLinkLocalUnicast(), n.IsLinkLocalMulticast(), n.IsMulticast():
		return fmt.Errorf("lan address %s is link-local or multicast — not a usable front-door address", a)
	case !isRFC1918(n):
		return fmt.Errorf("lan address %s is public; lan is a private (RFC1918) address — a public address is wan", a)
	}
	return nil
}

// normalizeAddr collapses an IPv4-in-IPv6 mapped address (::ffff:a.b.c.d) to
// its plain IPv4 form so no comparison is ever fooled by the mapped shape — the
// exact bypass a dual-stack v6 socket enables.
func normalizeAddr(a netip.Addr) netip.Addr {
	if a.Is4In6() {
		return a.Unmap()
	}
	return a
}

// VerifyBound checks the front-door addresses that actually bound (from
// getsockname) against the plan, fail-closed: every bound listener must be
// in the plan, and no wildcard may appear outside wan. Fewer listeners than
// the plan is never a violation — an empty sites directory binds nothing,
// and less exposure is always allowed. Addresses are normalized first, so
// an IPv4-in-IPv6 mapped form can never slip past as something other than
// its plain self.
func VerifyBound(scope Scope, lan netip.Addr, bound []netip.AddrPort) error {
	plan, err := PlanListeners(scope, lan)
	if err != nil {
		return err
	}
	want := map[netip.AddrPort]struct{}{}
	for _, s := range plan {
		want[normalizeAddrPort(s.Addr)] = struct{}{}
	}

	var violations []string
	for _, ap := range bound {
		n := normalizeAddrPort(ap)
		if n.Addr().IsUnspecified() && scope != ScopeWAN {
			violations = append(violations, fmt.Sprintf("wildcard listener %s where %s allows none", ap, scope))
			continue
		}
		if _, ok := want[n]; !ok {
			violations = append(violations, fmt.Sprintf("unexpected listener %s not in the %s plan", ap, scope))
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("exposure verification failed for scope %s: %s", scope, strings.Join(violations, "; "))
	}
	return nil
}

// BindPlan returns the addresses Caddy's default_bind should listen on for a
// scope on a given OS (goos = runtime.GOOS): what JANUS_BIND carries. macOS
// cannot bind an exact low port unprivileged, so every mode binds the
// wildcard there and the host firewall (pf) enforces the scope — the mode is
// invisible to the Caddyfile. Linux and Windows bind the exact addresses
// directly.
func BindPlan(scope Scope, goos string, lan netip.Addr) ([]string, error) {
	if _, err := PlanListeners(scope, lan); err != nil {
		return nil, err // validates inputs, e.g. lan requires an interface address
	}
	if goos == "darwin" {
		return []string{"0.0.0.0", "::"}, nil
	}
	switch scope {
	case ScopeLocalhost:
		return []string{"127.0.0.1", "::1"}, nil
	case ScopeLAN:
		return []string{"127.0.0.1", "::1", normalizeAddr(lan).String()}, nil
	case ScopeWAN:
		return []string{"0.0.0.0", "::"}, nil
	}
	return nil, fmt.Errorf("unknown exposure mode %q", scope)
}

func normalizeAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(normalizeAddr(ap.Addr()), ap.Port())
}

// Reach says how an address's reachability is enforced, so status never
// lets a mode name overclaim: the loopbacks and a private address by the
// host's addressing alone, the wildcard by nothing but the network.
type Reach string

const (
	ReachLoopback Reach = "host-enforced (loopback)"
	ReachRFC1918  Reach = "host-enforced (RFC1918)"
	ReachWildcard Reach = "network-gated (wildcard)" // wan
)

var rfc1918 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

func isRFC1918(a netip.Addr) bool {
	a = normalizeAddr(a)
	for _, p := range rfc1918 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// classifyReach labels one planned address.
func classifyReach(addr netip.Addr) Reach {
	a := normalizeAddr(addr)
	switch {
	case a.IsUnspecified():
		return ReachWildcard
	case a.IsLoopback():
		return ReachLoopback
	}
	return ReachRFC1918
}
