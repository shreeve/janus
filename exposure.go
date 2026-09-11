package janus

// The edge's exposure mode, as the process that runs it knows it. The
// service edge publishes the stored mode here when it starts and on every
// tick of its exposure watch. A standalone configuration publishes no
// service mode and supplies its own listener policy.
//
// In wan mode the local name families are not served, not certified, and
// not announced: <name>.local, <name>.localhost, localhost, and via.rip
// with <name>.via.rip, which public DNS pins to 127.0.0.1. They are names
// for a machine you sit at and a network you are on; a public address has
// neither, and a request that carries one in its Host header is someone
// asking the wrong question.

import (
	"net/netip"
	"strings"
	"sync/atomic"
)

type exposureState struct {
	scope   string
	iface   string
	lanIPv4 netip.Addr
}

var exposure atomic.Value // exposureState

// SetExposure publishes the service's validated scope and LAN selection
// together, so discovery cannot combine a new mode with an old address.
func SetExposure(scope, iface string, lanIPv4 netip.Addr) {
	exposure.Store(exposureState{scope: scope, iface: iface, lanIPv4: lanIPv4})
}

func currentExposure() exposureState {
	s, _ := exposure.Load().(exposureState)
	return s
}

// ExposureScope is the published mode, "" when nothing published one.
func ExposureScope() string {
	return currentExposure().scope
}

func wanExposure() bool { return ExposureScope() == "wan" }

// localFamilyHost reports whether host belongs to the local name families.
func localFamilyHost(host string) bool {
	return host == "localhost" || host == "via.rip" ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".via.rip")
}
