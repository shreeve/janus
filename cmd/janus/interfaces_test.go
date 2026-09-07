package main

import (
	"net/netip"
	"strings"
	"testing"
)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// pop's en0: one private IPv4 address (its IPv6 addresses are not
// candidates — lan is IPv4).
func popFacts() ifaceFacts {
	return ifaceFacts{name: "en0", up: true, nets: []netip.Prefix{pfx("10.0.0.211/24")}}
}

func TestChooseLANSingleAddress(t *testing.T) {
	lan, on, err := chooseLAN(popFacts(), "")
	if err != nil {
		t.Fatal(err)
	}
	if lan != mustAddr("10.0.0.211") || on != pfx("10.0.0.0/24") {
		t.Errorf("lan %s onlink %s", lan, on)
	}
	// Naming it is fine; naming something else is not.
	if lan, _, err := chooseLAN(popFacts(), "10.0.0.211"); err != nil || lan != mustAddr("10.0.0.211") {
		t.Errorf("named: %s %v", lan, err)
	}
	if _, _, err := chooseLAN(popFacts(), "10.0.0.9"); err == nil {
		t.Error("an address the interface does not have was accepted")
	}
	if _, _, err := chooseLAN(popFacts(), "not-an-ip"); err == nil {
		t.Error("garbage --v4 accepted")
	}
}

// Two private addresses on one interface: the operator chooses.
func TestChooseLANRequiresChoice(t *testing.T) {
	f := ifaceFacts{name: "eth0", up: true, nets: []netip.Prefix{pfx("10.0.0.249/24"), pfx("192.168.50.7/24")}}
	_, _, err := chooseLAN(f, "")
	if err == nil || !strings.Contains(err.Error(), "--v4") || !strings.Contains(err.Error(), "192.168.50.7/24") {
		t.Fatalf("ambiguous v4: %v", err)
	}
	lan, on, err := chooseLAN(f, "192.168.50.7")
	if err != nil || lan != mustAddr("192.168.50.7") || on != pfx("192.168.50.0/24") {
		t.Errorf("chosen: %s %s %v", lan, on, err)
	}
}

// A public address is never lan, and the refusal names it.
func TestChooseLANRefusesPublic(t *testing.T) {
	f := ifaceFacts{name: "ens3", up: true, nets: []netip.Prefix{pfx("34.22.36.78/32")}}
	if _, _, err := chooseLAN(f, ""); err == nil || !strings.Contains(err.Error(), "34.22.36.78") || !strings.Contains(err.Error(), "wan") {
		t.Errorf("public only: %v", err)
	}
	if _, _, err := chooseLAN(f, "34.22.36.78"); err == nil {
		t.Error("a public address was accepted by name")
	}
}

// The on-link subnet is the most inclusive prefix containing the address;
// a /32 host route alone is not a subnet.
func TestChooseLANOnLink(t *testing.T) {
	f := ifaceFacts{name: "eth0", up: true, nets: []netip.Prefix{pfx("10.0.0.249/32"), pfx("10.0.0.249/24")}}
	if _, on, err := chooseLAN(f, "10.0.0.249"); err != nil || on != pfx("10.0.0.0/24") {
		t.Errorf("sibling prefix: %s %v", on, err)
	}
	f.nets = []netip.Prefix{pfx("10.0.0.249/32")}
	if _, _, err := chooseLAN(f, ""); err == nil || !strings.Contains(err.Error(), "/32") {
		t.Errorf("lone /32: %v", err)
	}
}

func TestChooseLANExclusions(t *testing.T) {
	for _, name := range []string{"utun3", "bridge100", "vmenet0", "docker0", "awdl0", "llw0", "feth1"} {
		f := ifaceFacts{name: name, up: true, nets: []netip.Prefix{pfx("192.168.64.1/24")}}
		if _, _, err := chooseLAN(f, ""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	down := ifaceFacts{name: "en1", up: false, nets: []netip.Prefix{pfx("192.168.1.5/24")}}
	if _, _, err := chooseLAN(down, ""); err == nil {
		t.Error("a down interface accepted")
	}
	// Only loopback and link-local: nothing usable.
	lo := ifaceFacts{name: "en2", up: true, nets: []netip.Prefix{pfx("127.0.0.1/8"), pfx("169.254.3.4/16")}}
	if _, _, err := chooseLAN(lo, ""); err == nil {
		t.Error("loopback/link-local only accepted")
	}
	// 172.16/12 is private too.
	v4 := ifaceFacts{name: "eth0", up: true, nets: []netip.Prefix{pfx("172.16.5.9/16")}}
	lan, on, err := chooseLAN(v4, "")
	if err != nil || lan != mustAddr("172.16.5.9") || on != pfx("172.16.0.0/16") {
		t.Errorf("172.16: %s %s %v", lan, on, err)
	}
}

// The real reader reports only IPv4 addresses with their prefixes; the
// loopback interface is the one every host has.
func TestInterfaceFactsReadsIPv4(t *testing.T) {
	for _, name := range []string{"lo0", "lo"} {
		f, err := interfaceFacts(name)
		if err != nil {
			continue
		}
		for _, n := range f.nets {
			if !n.Addr().Is4() {
				t.Errorf("%s reported %s", name, n)
			}
		}
		return
	}
	t.Skip("no loopback interface by a known name")
}

// The default route's interface, as macOS and Linux report it.
func TestDefaultRouteParsers(t *testing.T) {
	mac := "   route to: default\ndestination: default\n       mask: default\n    gateway: 10.0.0.1\n  interface: en0\n      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>\n"
	if got, err := parseRouteGet(mac); err != nil || got != "en0" {
		t.Errorf("route get: %q %v", got, err)
	}
	if _, err := parseRouteGet("route: writing to routing socket: not in table\n"); err == nil || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("no default route: %v", err)
	}
	linux := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlp0s20f3\t00000000\t0100000A\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0000000A\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"
	if got, err := parseProcRoute(linux); err != nil || got != "eth0" {
		t.Errorf("proc route (lowest metric): %q %v", got, err)
	}
	if _, err := parseProcRoute("Iface\tDestination\n"); err == nil {
		t.Error("empty table gave an interface")
	}
}
