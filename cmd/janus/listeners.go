package main

// The edge's own assertion: what it bound is within what the scope allows.
// This is the one exposure fact an unprivileged process can observe about
// itself, so it is the one the edge asserts — the firewall is asserted by the
// verbs that manage it. The watch runs for the life of the process, so a
// reload that widened the bind is caught the same way a start is.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/shreeve/janus"
	"go.uber.org/zap"
)

// boundSocket is one bound IP socket of this process.
type boundSocket struct {
	proto string // tcp (listening) or udp
	addr  netip.AddrPort
}

func isFrontDoorPort(port uint16) bool {
	for _, p := range edgePorts {
		if p == port {
			return true
		}
	}
	return false
}

// verifyExposure checks the process's sockets against the scope on an OS.
// TCP on a front-door port must be in the plan — fewer is fine, more or
// wider is not. UDP on the front door is the webtransport relay's alone:
// port 443 at the plan's HTTPS addresses (fewer is fine again), never port
// 80, never an address outside the plan — HTTP/3, or a relay on a wider
// bind, would be exactly that. On macOS the socket is wildcard by design
// and pf is the scope truth, so nothing is judged by address there: TCP is
// not judged at all and UDP by port alone. UDP on other ports (mdns) is not
// a front-door concern.
func verifyExposure(st scopeState, goos string, socks []boundSocket) error {
	var violations []string
	var tcp, udp []netip.AddrPort
	for _, s := range socks {
		if !isFrontDoorPort(s.addr.Port()) {
			continue
		}
		switch {
		case s.proto != "udp":
			tcp = append(tcp, s.addr)
		case s.addr.Port() != 443:
			violations = append(violations, udpViolation(s.addr))
		default:
			udp = append(udp, s.addr)
		}
	}
	if goos != "darwin" {
		if err := VerifyBound(st.Scope, st.lan(), tcp); err != nil {
			violations = append(violations, err.Error())
		}
		if plan, err := PlanListeners(st.Scope, st.lan()); err == nil {
			want := map[netip.AddrPort]struct{}{}
			for _, l := range plan {
				if l.Role == RoleHTTPS {
					want[normalizeAddrPort(l.Addr)] = struct{}{}
				}
			}
			for _, a := range udp {
				if _, ok := want[normalizeAddrPort(a)]; !ok {
					violations = append(violations, udpViolation(a))
				}
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("%s", strings.Join(violations, "; "))
	}
	return nil
}

// udpViolation names the one capability that may open UDP on the front
// door, so an operator reading the log knows where the socket came from.
func udpViolation(a netip.AddrPort) string {
	return fmt.Sprintf("UDP listener on %s: only the webtransport relay may listen on UDP 443, and only at the mode's addresses", a)
}

// exposureWatch verifies the edge's sockets against scope.json — read
// again each time, so a mode change is judged by the mode now stored — at
// start and every two seconds until the context ends. A violation seen on
// two consecutive checks (a reload onto a narrower bind is in flight for a
// moment) is logged and ends the process with a failure exit, which the
// supervisor answers with a restart: the edge stays down, loudly, until the
// Caddyfile or the scope is fixed. An unreadable scope ends it the same way.
func exposureWatch(ctx context.Context, p servicePaths) {
	logger := caddy.Log().Named("janus.exposure")
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	strikes := 0
	for {
		st, err := readScope(p)
		if err != nil {
			logger.Error("the exposure mode is unreadable; refusing to serve unverified", zap.Error(err))
			exitExposure()
		}
		janus.SetExposure(string(st.Scope), st.Interface, st.lan())
		socks, err := ownSockets()
		if err != nil {
			logger.Error("cannot read the edge's own sockets; refusing to serve unverified", zap.Error(err))
			exitExposure()
		}
		if err := verifyExposure(st, runtime.GOOS, socks); err != nil {
			strikes++
			if strikes >= 2 {
				logger.Error("the edge's sockets do not match its exposure mode; stopping",
					zap.String("scope", string(st.Scope)), zap.Error(err),
					zap.String("fix", "edit the Caddyfile (default_bind {$JANUS_BIND}), or 'janus mode <scope>', then 'janus start'"))
				exitExposure()
			}
		} else {
			strikes = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// exitExposure is how the watch ends the process; a variable so tests can
// observe it instead.
var exitExposure = func() {
	_ = caddy.Stop()
	os.Exit(1)
}

// foreignListener reports an address on the plan that something else
// already answers on: the edge refuses to start beside it rather than share
// a port (Caddy's listeners carry SO_REUSEPORT, so a bind would not fail).
// With a wildcard bind the loopbacks are probed, which every wildcard
// answers.
func foreignListener(st scopeState, goos string, dial func(string, time.Duration) (net.Conn, error)) error {
	plan, err := PlanListeners(st.Scope, st.lan())
	if err != nil {
		return err
	}
	probe := map[netip.AddrPort]struct{}{}
	for _, l := range plan {
		a := l.Addr
		if a.Addr().IsUnspecified() || goos == "darwin" {
			a = netip.AddrPortFrom(loopbackAddrs()[0], l.Addr.Port())
			if l.Addr.Addr().Is6() {
				a = netip.AddrPortFrom(loopbackAddrs()[1], l.Addr.Port())
			}
		}
		probe[a] = struct{}{}
	}
	var held []string
	for a := range probe {
		if c, err := dial(a.String(), 300*time.Millisecond); err == nil {
			_ = c.Close()
			held = append(held, a.String())
		}
	}
	if len(held) > 0 {
		sort.Strings(held)
		return fmt.Errorf("another process already answers on %s; the edge refuses to start beside it", strings.Join(held, ", "))
	}
	return nil
}

// dialTCP is the probe's dialer; a variable so tests never touch this
// host's real ports.
var dialTCP = func(addr string, d time.Duration) (net.Conn, error) { return net.DialTimeout("tcp", addr, d) }
