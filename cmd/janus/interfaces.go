package main

// Interface resolution for lan: which interface carries the front door,
// which of its private IPv4 addresses the edge listens on, and which on-link
// subnet the macOS firewall admits. The interface is the default route's —
// the one this host reaches its network through — unless the operator names
// another. The address choice is deterministic and refuses ambiguity: when
// more than one candidate remains the operator names one. The platform
// supplies the raw facts; the rest is pure and tested with fakes.

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// excludedInterfaces never carry a front door: tunnels, bridges, VM and
// container fabrics, and Apple's peer-to-peer links.
var excludedInterfaces = regexp.MustCompile(`^(utun|bridge|vmenet|docker|awdl|llw|feth)`)

// ifaceFacts is what the platform reports about one interface.
type ifaceFacts struct {
	name string
	up   bool
	nets []netip.Prefix // each IPv4 address with its own prefix length
}

// interfaceFacts reads an interface from the OS; a variable so tests can
// stand in fakes.
var interfaceFacts = func(name string) (ifaceFacts, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return ifaceFacts{}, fmt.Errorf("interface %q: %v", name, err)
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return ifaceFacts{}, fmt.Errorf("interface %q addresses: %v", name, err)
	}
	f := ifaceFacts{name: name, up: ifi.Flags&net.FlagUp != 0}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		addr = normalizeAddr(addr)
		if !addr.Is4() {
			continue
		}
		bits, _ := ipn.Mask.Size()
		if bits > 32 {
			bits -= 96 // a 4-in-6 mask on a v4 address
		}
		f.nets = append(f.nets, netip.PrefixFrom(addr, bits))
	}
	return f, nil
}

// chooseLAN picks the lan address and its on-link subnet for an interface.
// want is the operator's explicit choice when the interface offers more
// than one private address.
func chooseLAN(f ifaceFacts, want string) (netip.Addr, netip.Prefix, error) {
	if excludedInterfaces.MatchString(f.name) {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("%s is a tunnel, bridge, or virtual link, never a front door; name a real interface", f.name)
	}
	if !f.up {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("%s is not up", f.name)
	}
	var cands []netip.Prefix
	var public []string
	for _, n := range f.nets {
		a := n.Addr()
		if !a.Is4() || a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() {
			continue
		}
		if !isRFC1918(a) {
			public = append(public, a.String())
			continue
		}
		cands = append(cands, n)
	}
	addr, err := pickOne(f.name, cands, want, public)
	if err != nil {
		return netip.Addr{}, netip.Prefix{}, err
	}
	onlink, err := onLinkFor(f.name, addr, f.nets)
	if err != nil {
		return netip.Addr{}, netip.Prefix{}, err
	}
	return addr, onlink, nil
}

// pickOne resolves the address: the single candidate, the operator's named
// one (which must be a candidate), or an error that lists the choices.
func pickOne(iface string, cands []netip.Prefix, want string, public []string) (netip.Addr, error) {
	if want != "" {
		a, err := netip.ParseAddr(want)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("--v4 %q: %v", want, err)
		}
		a = normalizeAddr(a)
		for _, c := range cands {
			if c.Addr() == a {
				return a, nil
			}
		}
		return netip.Addr{}, fmt.Errorf("--v4 %s is not a private IPv4 address of %s; %s", a, iface, listCandidates(cands))
	}
	switch len(cands) {
	case 0:
		if len(public) > 0 {
			sort.Strings(public)
			return netip.Addr{}, fmt.Errorf("%s has no private IPv4 address, only %s; a public address is wan, not lan", iface, strings.Join(public, ", "))
		}
		return netip.Addr{}, fmt.Errorf("%s has no usable IPv4 address (loopback and link-local do not count)", iface)
	case 1:
		return cands[0].Addr(), nil
	}
	return netip.Addr{}, fmt.Errorf("%s has %d private IPv4 addresses; choose one with --v4 <address>: %s", iface, len(cands), listCandidates(cands))
}

func listCandidates(cands []netip.Prefix) string {
	if len(cands) == 0 {
		return "it has none"
	}
	items := make([]string, 0, len(cands))
	for _, c := range cands {
		items = append(items, c.String())
	}
	sort.Strings(items)
	return strings.Join(items, ", ")
}

// onLinkFor is the most inclusive prefix on the interface that contains the
// chosen address: the subnet the firewall admits. A host route alone (a
// /32) is not a subnet and is refused.
func onLinkFor(iface string, a netip.Addr, nets []netip.Prefix) (netip.Prefix, error) {
	best := netip.Prefix{}
	for _, n := range nets {
		if !n.Addr().Is4() || !n.Contains(a) {
			continue
		}
		if !best.IsValid() || n.Bits() < best.Bits() {
			best = n
		}
	}
	if !best.IsValid() || best.Bits() >= 32 {
		return netip.Prefix{}, fmt.Errorf("%s has no on-link prefix for %s (only a /32 host route); the subnet is needed", iface, a)
	}
	return best.Masked(), nil
}

// defaultInterface is the interface the IPv4 default route leaves through;
// a variable so tests can stand in a fake.
var defaultInterface = func() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("/sbin/route", "-n", "get", "default").Output()
		if err != nil {
			return "", errors.New("no IPv4 default route; name the interface with --interface")
		}
		return parseRouteGet(string(out))
	case "linux":
		b, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return "", err
		}
		return parseProcRoute(string(b))
	}
	return "", fmt.Errorf("no default-route lookup on %s; name the interface with --interface", runtime.GOOS)
}

// parseRouteGet reads the interface line of `route -n get default`.
func parseRouteGet(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "interface" {
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("no IPv4 default route; name the interface with --interface")
}

// parseProcRoute picks the default route (destination 0.0.0.0) with the
// lowest metric from /proc/net/route.
func parseProcRoute(table string) (string, error) {
	best, bestMetric := "", -1
	for i, line := range strings.Split(table, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 7 || f[1] != "00000000" {
			continue
		}
		metric := 0
		fmt.Sscanf(f[6], "%d", &metric)
		if best == "" || metric < bestMetric {
			best, bestMetric = f[0], metric
		}
	}
	if best == "" {
		return "", errors.New("no IPv4 default route; name the interface with --interface")
	}
	return best, nil
}
