package janus

// The edge's exposure mode, as the process that runs it knows it. The
// service edge publishes the stored mode here when it starts and on every
// tick of its exposure watch; a config loaded some other way publishes
// nothing, and nothing means the default, localhost.
//
// In wan mode the local name families are not served, not certified, and
// not announced: <name>.local, <name>.localhost, localhost, and via.rip
// with <name>.via.rip, which public DNS pins to 127.0.0.1. They are names
// for a machine you sit at and a network you are on; a public address has
// neither, and a request that carries one in its Host header is someone
// asking the wrong question.

import (
	"strings"
	"sync/atomic"
)

var exposureScope atomic.Value // string

// SetExposureScope publishes the edge's exposure mode to the module.
func SetExposureScope(scope string) { exposureScope.Store(scope) }

// ExposureScope is the published mode, "" when nothing published one.
func ExposureScope() string {
	s, _ := exposureScope.Load().(string)
	return s
}

func wanExposure() bool { return ExposureScope() == "wan" }

// localFamilyHost reports whether host belongs to the local name families.
func localFamilyHost(host string) bool {
	return host == "localhost" || host == "via.rip" ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".via.rip")
}
