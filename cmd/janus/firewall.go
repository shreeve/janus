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
// The front door is TCP on 80 and 443 plus the webtransport relay's UDP on
// 443, the one UDP listener the mode covers. Source filtering is sound for
// TCP: a completed handshake needs the SYN-ACK to reach the real on-link
// address, so an off-link peer cannot spoof its way in. A UDP source is
// spoofable, so the UDP allow only narrows who can knock; the QUIC
// handshake's source-address validation is the gate.

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
// IPv4. Port 80 is TCP alone; port 443 is `proto { tcp udp }`, the UDP for
// the webtransport relay. Rules are first-match (quick), so the allows come
// first and the blocks catch everything else on both ports.
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
//
// The module reproduces this text to check the installed anchor before
// the relay starts, so the function stays pure and exported.
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
	fmt.Fprintf(&b, "pass in quick inet proto tcp from { %s } to any port 80 flags any no state\n", strings.Join(v4, " "))
	fmt.Fprintf(&b, "pass in quick inet proto { tcp udp } from { %s } to any port 443 flags any no state\n", strings.Join(v4, " "))
	b.WriteString("pass in quick inet6 proto tcp from { ::1 } to any port 80 flags any no state\n")
	b.WriteString("pass in quick inet6 proto { tcp udp } from { ::1 } to any port 443 flags any no state\n")
	b.WriteString("block in quick proto tcp from any to any port 80\n")
	b.WriteString("block in quick proto { tcp udp } from any to any port 443\n")
	return b.String(), nil
}

// windowsRuleNames are the DisplayNames of the Defender rules a mode
// installs, TCP first: what a check looks up and a removal deletes, so both
// know every rule the mode ever wrote.
func windowsRuleNames(scope Scope) []string {
	switch scope {
	case ScopeLAN, ScopeWAN:
		return []string{fmt.Sprintf("Janus edge (%s)", scope), fmt.Sprintf("Janus webtransport (%s)", scope)}
	}
	return nil
}

// WindowsFirewallRules returns the New-NetFirewallRule invocations for a
// mode, one per rule: TCP 80,443 and the webtransport relay's UDP 443, under
// the same profiles. Defender blocks unsolicited inbound by default, so
// these open the door rather than restrict it: nothing for localhost
// (loopback is exempt), LocalSubnet-scoped inbound allows for lan, and
// all-profiles allows for wan.
func WindowsFirewallRules(scope Scope) []string {
	var where string
	switch scope {
	case ScopeLAN:
		where = "-Profile Private,Domain -RemoteAddress LocalSubnet"
	case ScopeWAN:
		where = "-Profile Any"
	default:
		return nil
	}
	names := windowsRuleNames(scope)
	return []string{
		fmt.Sprintf(`New-NetFirewallRule -DisplayName "%s" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 80,443 %s`, names[0], where),
		fmt.Sprintf(`New-NetFirewallRule -DisplayName "%s" -Direction Inbound -Action Allow -Protocol UDP -LocalPort 443 %s`, names[1], where),
	}
}
