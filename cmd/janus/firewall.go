package main

// Firewall rules that enforce an exposure scope where binding alone cannot.
// A rule is needed for lan on every platform (the interface's IPv6 address is
// globally routable, so only an on-link source filter keeps the internet out),
// for localhost on macOS only (where the socket is wildcard and pf stands in
// for an exact bind), and for wan never. The generators here are pure: they
// produce ruleset text for the privileged service path to parse-check (pfctl
// -nf, nft -c) and install. Nothing here applies a rule.
//
// Source filtering is sound for TCP: a completed handshake needs the SYN-ACK to
// reach the real on-link address, so an off-link peer cannot spoof its way in.

import (
	"fmt"
	"net/netip"
	"strings"
)

// OnLink carries the host's current on-link prefixes — its IPv4 subnet and its
// IPv6 /64 — the source ranges a lan rule admits. They are re-derived whenever
// the interface address changes (DHCP/SLAAC drift), never assumed.
type OnLink struct {
	V4 netip.Prefix
	V6 netip.Prefix
}

// NeedsFirewall reports whether a scope requires a host firewall rule on the
// given OS. It encodes the settled matrix: lan everywhere, localhost on macOS
// only, wan never.
func NeedsFirewall(scope Scope, goos string) bool {
	switch scope {
	case ScopeLAN:
		return true
	case ScopeLocalhost:
		return goos == "darwin"
	}
	return false
}

// PfAnchor returns the macOS pf anchor that scopes the wildcard socket to a
// mode: loopback only for localhost, loopback + the on-link prefixes (and
// link-local) for lan, and nothing for wan. Rules are first-match (quick), so
// the allows come first and the block catches everything else.
func PfAnchor(scope Scope, onlink OnLink) (string, error) {
	v4 := []string{"127.0.0.0/8"}
	v6 := []string{"::1"}
	switch scope {
	case ScopeWAN:
		return "", nil
	case ScopeLocalhost:
		// loopback only
	case ScopeLAN:
		v6 = append(v6, "fe80::/10")
		if onlink.V4.IsValid() {
			v4 = append(v4, onlink.V4.String())
		}
		if onlink.V6.IsValid() {
			v6 = append(v6, onlink.V6.String())
		}
	default:
		return "", fmt.Errorf("unknown exposure mode %q", scope)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# janus exposure anchor - scope %s\n", scope)
	fmt.Fprintf(&b, "pass in quick inet proto tcp from { %s } to any port { 80 443 }\n", strings.Join(v4, " "))
	fmt.Fprintf(&b, "pass in quick inet6 proto tcp from { %s } to any port { 80 443 }\n", strings.Join(v6, " "))
	b.WriteString("block in quick proto tcp from any to any port { 80 443 }\n")
	return b.String(), nil
}

// NftRules returns the Linux nftables ruleset for lan: the interface's IPv6
// address is globally routable, so inbound :80/:443 over IPv6 is restricted to
// on-link sources. The IPv4 address is a bound RFC1918 address and needs no
// rule. localhost and wan return nothing — Linux binds those exactly.
func NftRules(scope Scope, onlink OnLink) (string, error) {
	if scope != ScopeLAN {
		return "", nil
	}
	v6 := []string{"::1", "fe80::/10"}
	if onlink.V6.IsValid() {
		v6 = append(v6, onlink.V6.String())
	}
	var b strings.Builder
	b.WriteString("table inet janus_exposure {\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0;\n")
	fmt.Fprintf(&b, "\t\ttcp dport { 80, 443 } ip6 saddr { %s } accept\n", strings.Join(v6, ", "))
	b.WriteString("\t\ttcp dport { 80, 443 } meta nfproto ipv6 drop\n")
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

// WindowsFirewallRule returns the New-NetFirewallRule invocation for a mode.
// Defender blocks unsolicited inbound by default, so these open the door rather
// than restrict it: nothing for localhost (loopback is exempt), a
// LocalSubnet-scoped inbound allow for lan (which also bounds the global IPv6
// address to the local link, tracking DHCP on its own), and an all-profiles
// allow for wan.
func WindowsFirewallRule(scope Scope) string {
	switch scope {
	case ScopeLAN:
		return `New-NetFirewallRule -DisplayName "Janus edge (lan)" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 80,443 -Profile Private,Domain -RemoteAddress LocalSubnet`
	case ScopeWAN:
		return `New-NetFirewallRule -DisplayName "Janus edge (wan)" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 80,443 -Profile Any`
	}
	return ""
}
