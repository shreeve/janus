package janus

import (
	"net"
	"net/netip"
	"testing"

	"github.com/brutella/dnssd"
)

// Inspect the actual DNS records produced from the responder's services,
// including a host with multiple addresses and interfaces on the machine.
func TestMdnsLANRecordsFollowExposure(t *testing.T) {
	t.Cleanup(func() { SetExposure("", "", netip.Addr{}) })
	SetExposure("lan", "en0", netip.MustParseAddr("10.0.0.211"))
	reg := newAppRegistry()
	if _, err := reg.create("shop", []string{"shop.local"}, ""); err != nil {
		t.Fatal(err)
	}
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	t.Cleanup(func() { _ = adv.configure(nil, nil); adv.reconcile() })
	cfg := &mdnsConfig{name: "janus.local", port: 80, apps: true}
	if err := adv.configure(t, cfg); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	check := func(iface, address string) {
		t.Helper()
		for _, host := range []string{"janus", "shop"} {
			srv := fake.handles[host].Service()
			if len(srv.Ifaces) != 1 || srv.Ifaces[0] != iface || srv.IsVisibleAtInterface("other0") {
				t.Fatalf("%s interfaces = %v, want only %s", host, srv.Ifaces, iface)
			}
			nic := &net.Interface{Name: iface}
			a := dnssd.A(srv, nic)
			if len(a) != 1 || a[0].A.String() != address {
				t.Errorf("%s A = %v, want only %s", host, a, address)
			}
			if aaaa := dnssd.AAAA(srv, nic); len(aaaa) != 0 {
				t.Errorf("%s published unreachable AAAA records: %v", host, aaaa)
			}
			if srv.AdvertiseIPType != dnssd.IPv4 {
				t.Errorf("%s allows IPv6 advertisements", host)
			}
		}
	}
	check("en0", "10.0.0.211")
	// An unchanged watcher publication and config reload must not flap.
	SetExposure("lan", "en0", netip.MustParseAddr("10.0.0.211"))
	if err := adv.configure(new(int), cfg); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if adv.announces.Load() != 2 || adv.withdraws.Load() != 0 {
		t.Fatal("unchanged LAN selection flapped advertisements")
	}
	// A DHCP change must replace records even if the names/config are equal.
	SetExposure("lan", "en0", netip.MustParseAddr("10.0.0.212"))
	adv.reconcile()
	check("en0", "10.0.0.212")
	if adv.announces.Load() != 4 || adv.withdraws.Load() != 2 {
		t.Fatal("address change did not withdraw and replace both names")
	}
	SetExposure("lan", "en1", netip.MustParseAddr("192.168.1.2"))
	adv.reconcile()
	check("en1", "192.168.1.2")
	for _, scope := range []string{"localhost", "wan"} {
		SetExposure(scope, "", netip.Addr{})
		adv.reconcile()
		if got := adv.snapshot("janus.local"); len(got.entries) != 0 {
			t.Fatalf("%s advertised unreachable LAN names: %+v", scope, got)
		}
	}
	SetExposure("lan", "en0", netip.MustParseAddr("10.0.0.211"))
	adv.reconcile()
	check("en0", "10.0.0.211")
}

func TestMdnsLANRespectsPinnedInterfaces(t *testing.T) {
	SetExposure("lan", "en0", netip.MustParseAddr("10.0.0.211"))
	t.Cleanup(func() { SetExposure("", "", netip.Addr{}) })
	app := newTestSharedMdnsApp(t)
	app.Mdns.Interfaces = []string{"other0"}
	if err := app.startMdns(); err == nil {
		t.Fatal("accepted pinned interfaces that exclude the LAN interface")
	}
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, nil, fake)
	t.Cleanup(func() { _ = adv.configure(nil, nil); adv.reconcile() })
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, ifaces: []string{"en0"}}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	SetExposure("lan", "other0", netip.MustParseAddr("192.168.1.2"))
	adv.reconcile()
	if len(adv.snapshot("janus.local").entries) != 0 {
		t.Fatal("mode change advertised on an interface excluded by cold config")
	}
}

func TestMdnsStandaloneKeepsAddressPolicy(t *testing.T) {
	SetExposure("", "", netip.Addr{})
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, nil, fake)
	t.Cleanup(func() { _ = adv.configure(nil, nil); adv.reconcile() })
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, ifaces: []string{"custom0"}}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	srv := fake.handles["janus"].Service()
	if len(srv.IPs) != 0 || srv.AdvertiseIPType != dnssd.Both || len(srv.Ifaces) != 1 || srv.Ifaces[0] != "custom0" {
		t.Fatalf("standalone address policy changed: %+v", srv)
	}
}
