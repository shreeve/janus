package main

// Firewall rules that enforce an exposure scope where binding alone cannot.
// That is macOS, whose socket is the wildcard in every mode: pf scopes it to
// the loopbacks (localhost) or to the loopbacks plus the on-link IPv4 subnet
// (lan), and wan needs nothing. Linux binds the exact addresses and lan's
// address is private, so no rule is needed there. Windows Defender blocks
// unsolicited inbound by default, so lan and wan need an allow rule to open
// the door — that is opening, not scoping. The generators here are pure:
// they produce ruleset text for the privileged service path to parse-check
// and install. Nothing here applies a rule.
//
// Source filtering is sound for TCP: a completed handshake needs the SYN-ACK to
// reach the real on-link address, so an off-link peer cannot spoof its way in.

import (
	"fmt"
	"net/netip"
	"strings"
)

// NeedsFirewall reports whether a scope requires a host firewall rule on the
// given OS: every mode but wan on macOS, none anywhere else.
func NeedsFirewall(scope Scope, goos string) bool {
	return goos == "darwin" && scope != ScopeWAN
}

// PfAnchor returns the macOS pf anchor that scopes the wildcard socket to a
// mode: the loopbacks for localhost, the loopbacks plus the on-link IPv4
// subnet for lan, and nothing for wan. IPv6 passes from ::1 only — lan is
// IPv4. Rules are first-match (quick), so the allows come first and the block
// catches everything else.
//
// The rules are stateless (flags any, no state): every packet is judged by
// the rules loaded now, so narrowing the mode cuts connections that the
// wider mode admitted, and connections that predate the rules are not
// grandfathered in. The block is `to any`: it covers every address the
// host has and every one it gains later, and it also stops traffic the
// host forwards for others on these two ports (Internet Sharing, VMs on
// shared networking). macOS pf offers nothing narrower that holds: `self`
// is a load-time snapshot that misses a new address, and `(self)`, the
// dynamic form, parses but is never populated by the macOS kernel (the
// block matched nothing on a live host), so either would fail open.
func PfAnchor(scope Scope, onlink netip.Prefix) (string, error) {
	v4 := []string{"127.0.0.0/8"}
	switch scope {
	case ScopeWAN:
		return "", nil
	case ScopeLocalhost:
		// loopback only
	case ScopeLAN:
		if !onlink.IsValid() || !onlink.Addr().Is4() {
			return "", fmt.Errorf("lan needs the interface's on-link IPv4 prefix")
		}
		v4 = append(v4, onlink.Masked().String())
	default:
		return "", fmt.Errorf("unknown exposure mode %q", scope)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# janus exposure anchor - scope %s\n", scope)
	fmt.Fprintf(&b, "pass in quick inet proto tcp from { %s } to any port { 80 443 } flags any no state\n", strings.Join(v4, " "))
	b.WriteString("pass in quick inet6 proto tcp from { ::1 } to any port { 80 443 } flags any no state\n")
	b.WriteString("block in quick proto tcp from any to any port { 80 443 }\n")
	return b.String(), nil
}

// WindowsFirewallRule returns the New-NetFirewallRule invocation for a mode.
// Defender blocks unsolicited inbound by default, so these open the door rather
// than restrict it: nothing for localhost (loopback is exempt), a
// LocalSubnet-scoped inbound allow for lan, and an all-profiles allow for wan.
func WindowsFirewallRule(scope Scope) string {
	switch scope {
	case ScopeLAN:
		return `New-NetFirewallRule -DisplayName "Janus edge (lan)" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 80,443 -Profile Private,Domain -RemoteAddress LocalSubnet`
	case ScopeWAN:
		return `New-NetFirewallRule -DisplayName "Janus edge (wan)" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 80,443 -Profile Any`
	}
	return ""
}
