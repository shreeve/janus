package main

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The kernel's answer for this process's own listeners includes one it
// just opened, as a listening TCP socket on the exact address.
func TestOwnSocketsSeesOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	want := netip.MustParseAddrPort(ln.Addr().String())
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer udp.Close()
	wantUDP := netip.MustParseAddrPort(udp.LocalAddr().String())
	// A connected client socket is not listening and must not appear.
	client, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	local := netip.MustParseAddrPort(client.LocalAddr().String())

	socks, err := ownSockets()
	if err != nil {
		t.Fatal(err)
	}
	var sawTCP, sawUDP bool
	for _, s := range socks {
		switch {
		case s.proto == "tcp" && s.addr == want:
			sawTCP = true
		case s.proto == "udp" && s.addr == wantUDP:
			sawUDP = true
		case s.proto == "tcp" && s.addr == local:
			t.Errorf("client socket %s reported as a listener", local)
		}
	}
	if !sawTCP || !sawUDP {
		t.Errorf("listener %s seen=%v, udp %s seen=%v, in %v", want, sawTCP, wantUDP, sawUDP, socks)
	}
}

func sock(proto, ap string) boundSocket {
	return boundSocket{proto: proto, addr: netip.MustParseAddrPort(ap)}
}

func TestVerifyExposure(t *testing.T) {
	local := scopeState{Scope: ScopeLocalhost}
	ok := []boundSocket{sock("tcp", "127.0.0.1:443"), sock("tcp", "[::1]:443"), sock("tcp", "127.0.0.1:7600"), sock("udp", "[::]:5353")}
	if err := verifyExposure(local, "linux", ok); err != nil {
		t.Errorf("linux exact bind: %v", err)
	}
	// Nothing bound on the front door (empty sites dir) is fine.
	if err := verifyExposure(local, "linux", nil); err != nil {
		t.Errorf("nothing bound: %v", err)
	}
	wide := append(ok, sock("tcp", "0.0.0.0:443"))
	if err := verifyExposure(local, "linux", wide); err == nil || !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("linux wildcard in localhost: %v", err)
	}
	// macOS binds the wildcard by design; pf is the scope truth there.
	if err := verifyExposure(local, "darwin", wide); err != nil {
		t.Errorf("darwin wildcard: %v", err)
	}
	// UDP on a front-door port is HTTP/3 leaking beside the scope, anywhere.
	h3 := []boundSocket{sock("tcp", "0.0.0.0:443"), sock("udp", "0.0.0.0:443")}
	for _, goos := range []string{"darwin", "linux"} {
		if err := verifyExposure(scopeState{Scope: ScopeWAN}, goos, h3); err == nil || !strings.Contains(err.Error(), "UDP") {
			t.Errorf("%s h3: %v", goos, err)
		}
	}
	// A lan edge bound on an address that is not its stored one.
	lan := lanState()
	if err := verifyExposure(lan, "linux", []boundSocket{sock("tcp", "10.0.0.211:443"), sock("tcp", "10.0.0.212:443")}); err == nil || !strings.Contains(err.Error(), "10.0.0.212:443") {
		t.Errorf("stray lan address: %v", err)
	}
}

// The watch ends the process on a violation and stays quiet otherwise. A
// UDP socket on a front-door port is a violation on every OS (HTTP/3
// beside scoped TCP), so this runs everywhere. The scope is read from disk
// each time: a violation must be seen twice, two seconds apart.
func TestExposureWatchExitsOnViolation(t *testing.T) {
	p := isolatedHome(t)
	if err := writeScope(p, lanState()); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{}, 1)
	prev := exitExposure
	exitExposure = func() {
		select {
		case exited <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { exitExposure = prev })
	udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer udp.Close()
	port := netip.MustParseAddrPort(udp.LocalAddr().String()).Port()
	prevPorts := edgePorts
	edgePorts = []uint16{port}
	t.Cleanup(func() { edgePorts = prevPorts })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	go exposureWatch(ctx, p)
	select {
	case <-exited:
		if time.Since(start) < 1500*time.Millisecond {
			t.Error("the watch exited on the first sighting; a reload in flight needs one tick of grace")
		}
	case <-ctx.Done():
		t.Fatal("the watch did not end the process on a violation")
	}
	// A clean process is left alone.
	udp.Close()
	exited2 := make(chan struct{}, 1)
	exitExposure = func() { exited2 <- struct{}{} }
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	go exposureWatch(ctx2, p)
	select {
	case <-exited2:
		t.Fatal("the watch ended a compliant process")
	case <-ctx2.Done():
	}
}

func TestForeignListener(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	port := netip.MustParseAddrPort(ln.Addr().String()).Port()
	prevPorts := edgePorts
	edgePorts = []uint16{port}
	t.Cleanup(func() { edgePorts = prevPorts })
	held := func(addr string, d time.Duration) (net.Conn, error) { return net.DialTimeout("tcp", addr, d) }
	err = foreignListener(scopeState{Scope: ScopeLocalhost}, "linux", held)
	if err == nil || !strings.Contains(err.Error(), ln.Addr().String()) {
		t.Errorf("held port not refused: %v", err)
	}
	// A wildcard plan (wan, or any mode on macOS) is probed on the loopbacks.
	if err := foreignListener(scopeState{Scope: ScopeWAN}, "linux", held); err == nil {
		t.Error("wan plan did not probe loopback")
	}
	if err := foreignListener(lanState(), "darwin", held); err == nil {
		t.Error("darwin plan did not probe loopback")
	}
	ln.Close()
	if err := foreignListener(scopeState{Scope: ScopeLocalhost}, "linux", held); err != nil {
		t.Errorf("free port refused: %v", err)
	}
}
