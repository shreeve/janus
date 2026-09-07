package main

// Exposure modes: the portable core of the localhost|lan|wan contract
// (see EXPOSURE.md). A mode is a list of exact addresses to listen on. This
// file is the shared spine every platform builds on — it plans the address
// list, decides what a bound listener is allowed to be, verifies what
// actually bound against the plan (fail-closed), and classifies each address
// so status can tell the truth about reachability. Acquiring the listeners
// (launchd sockets on macOS, capability binds on Linux, direct binds on
// Windows) is the one platform-specific step and lives elsewhere; everything
// here is deterministic and OS-independent.

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
	ScopeLAN       Scope = "lan"       // localhost + the selected interface's address(es)
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

// LANAddrs are the selected interface's chosen unicast addresses for lan mode.
// Either family may be zero (absent); at least one must be present.
type LANAddrs struct {
	V4 netip.Addr
	V6 netip.Addr
}

func loopbackAddrs() []netip.Addr {
	return []netip.Addr{
		netip.AddrFrom4([4]byte{127, 0, 0, 1}),
		netip.IPv6Loopback(),
	}
}

// PlanListeners turns a scope into the exact set of listeners to acquire. It is
// pure and deterministic — the heart of the contract. It rejects anything that
// would silently widen the scope: an unspecified address outside wan, a
// loopback/link-local/multicast/mapped address offered as a lan address, or a
// lan mode with no interface address at all (which must fail closed, not decay
// to localhost).
func PlanListeners(scope Scope, lan LANAddrs) ([]ListenerSpec, error) {
	var addrs []netip.Addr
	switch scope {
	case ScopeLocalhost:
		addrs = loopbackAddrs()
	case ScopeLAN:
		addrs = loopbackAddrs()
		for _, a := range []netip.Addr{lan.V4, lan.V6} {
			if !a.IsValid() {
				continue
			}
			if err := validLANAddr(a); err != nil {
				return nil, err
			}
			addrs = append(addrs, normalizeAddr(a))
		}
		if !lan.V4.IsValid() && !lan.V6.IsValid() {
			return nil, fmt.Errorf("lan mode requires at least one selected interface address")
		}
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

// validLANAddr rejects addresses that must never be offered as a lan address.
// Interface enumeration is expected to exclude these already; this is the
// fail-closed backstop.
func validLANAddr(a netip.Addr) error {
	n := normalizeAddr(a)
	switch {
	case !n.IsValid():
		return fmt.Errorf("lan address is not a valid IP")
	case n.IsUnspecified():
		return fmt.Errorf("lan address %s is unspecified (wildcard) — not allowed outside wan", a)
	case n.IsLoopback():
		return fmt.Errorf("lan address %s is loopback — loopback is bound implicitly", a)
	case n.IsLinkLocalUnicast(), n.IsLinkLocalMulticast(), n.IsMulticast():
		return fmt.Errorf("lan address %s is link-local or multicast — not a usable front-door address", a)
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

// AllowedSet is the set of local addresses a scope permits a listener to be
// bound to. WAN permits the two unspecified addresses; localhost and lan permit
// an explicit, closed set. Used to verify what actually bound.
type AllowedSet struct {
	scope Scope
	addrs map[netip.Addr]struct{}
}

// NewAllowedSet builds the allowed address set for a scope.
func NewAllowedSet(scope Scope, lan LANAddrs) AllowedSet {
	set := AllowedSet{scope: scope, addrs: map[netip.Addr]struct{}{}}
	add := func(a netip.Addr) {
		if a.IsValid() {
			set.addrs[normalizeAddr(a)] = struct{}{}
		}
	}
	switch scope {
	case ScopeLocalhost:
		for _, a := range loopbackAddrs() {
			add(a)
		}
	case ScopeLAN:
		for _, a := range loopbackAddrs() {
			add(a)
		}
		add(lan.V4)
		add(lan.V6)
	case ScopeWAN:
		add(netip.IPv4Unspecified())
		add(netip.IPv6Unspecified())
	}
	return set
}

// Contains reports whether an address is permitted in this scope. The address
// is normalized first, so a mapped v4 form is judged as its plain v4 self.
func (a AllowedSet) Contains(addr netip.Addr) bool {
	_, ok := a.addrs[normalizeAddr(addr)]
	return ok
}

// VerifyBound checks the addresses that actually bound (from getsockname)
// against the plan, fail-closed. It returns an error describing every
// violation: an address outside the allowed set, an unspecified address in a
// non-wan scope, a required listener missing, or an unexplained surplus
// listener. A nil return means the bound set exactly realizes the scope.
func VerifyBound(scope Scope, lan LANAddrs, bound []netip.AddrPort) error {
	plan, err := PlanListeners(scope, lan)
	if err != nil {
		return err
	}
	allowed := NewAllowedSet(scope, lan)

	want := map[netip.AddrPort]struct{}{}
	for _, s := range plan {
		want[normalizeAddrPort(s.Addr)] = struct{}{}
	}
	got := map[netip.AddrPort]struct{}{}

	var violations []string
	for _, ap := range bound {
		n := normalizeAddrPort(ap)
		got[n] = struct{}{}
		addr := n.Addr()
		if addr.IsUnspecified() && scope != ScopeWAN {
			violations = append(violations, fmt.Sprintf("wildcard listener %s where %s allows none", ap, scope))
			continue
		}
		if !allowed.Contains(addr) {
			violations = append(violations, fmt.Sprintf("listener %s is outside the %s allowed set", ap, scope))
		}
	}
	for ap := range want {
		if _, ok := got[ap]; !ok {
			violations = append(violations, fmt.Sprintf("required listener %s is missing", ap))
		}
	}
	for ap := range got {
		if _, ok := want[ap]; !ok {
			// Outside-allowed-set surplus is already reported above; only
			// flag an in-set address bound on an unexpected port here.
			if allowed.Contains(ap.Addr()) && !ap.Addr().IsUnspecified() {
				violations = append(violations, fmt.Sprintf("surplus listener %s not in the plan", ap))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("exposure verification failed for scope %s: %s", scope, strings.Join(violations, "; "))
	}
	return nil
}

func normalizeAddrPort(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(normalizeAddr(ap.Addr()), ap.Port())
}

// Reach classifies how an address's reachability is actually enforced, so
// status never lets a mode name overclaim. lan over IPv6 binds a globally
// routable address; only the on-link host firewall (not the host's addressing)
// keeps the internet out, so it is labeled distinctly from RFC1918 lan-v4.
type Reach string

const (
	ReachLoopback Reach = "host-enforced (loopback)"
	ReachRFC1918  Reach = "host-enforced (RFC1918)"
	ReachULA      Reach = "host-enforced (ULA)"
	ReachFirewall Reach = "host-enforced (firewall)"     // lan global address + on-link rule
	ReachRouter   Reach = "network-gated (router only)"  // global address, no host rule
	ReachWildcard Reach = "network-gated (wildcard)"     // wan
)

var (
	rfc1918 = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	ulaV6 = netip.MustParsePrefix("fc00::/7")
)

func isRFC1918(a netip.Addr) bool {
	a = normalizeAddr(a)
	for _, p := range rfc1918 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// classifyReach labels one bound address. lanFirewall reports whether the
// on-link host firewall rule that scopes lan to the local subnet is in force;
// it changes a global lan address from router-gated to host-enforced.
func classifyReach(scope Scope, addr netip.Addr, lanFirewall bool) Reach {
	a := normalizeAddr(addr)
	switch {
	case a.IsUnspecified():
		return ReachWildcard
	case a.IsLoopback():
		return ReachLoopback
	case isRFC1918(a):
		if scope == ScopeLAN && lanFirewall {
			return ReachRFC1918 // RFC1918 + firewall; addressing alone already suffices
		}
		return ReachRFC1918
	case ulaV6.Contains(a):
		return ReachULA
	default:
		// A global unicast address (typically IPv6 with no NAT). Its safety is
		// the host firewall if present, otherwise only the network's.
		if scope == ScopeLAN && lanFirewall {
			return ReachFirewall
		}
		return ReachRouter
	}
}
