package main

import (
	"context"
	"net"
	"strings"
)

// loopbackAddr sends an authority under the reserved localhost name to the
// loopback, as RFC 6761 makes it and as curl does, so a test host needs no
// resolver support: not every platform's resolver (or Go's, built without
// the system one) maps *.localhost. Any other authority is returned as is.
func loopbackAddr(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return hostport
}

// dialLoopback is a DialContext that applies loopbackAddr.
func dialLoopback(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, loopbackAddr(addr))
}
