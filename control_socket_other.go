//go:build !unix || solaris

package janus

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/caddyserver/caddy/v2"
)

// On these platforms caddy's unix socket reuse rides its refcounted
// listenerPool (listen.go), whose shared entry is stable across
// generations — the aborted-reload poison lives only in the
// unix-and-not-solaris map implementation. Delegate to caddy unchanged.
type controlSocketPool struct {
	mu sync.Mutex // parity with the unix implementation; nothing to guard
}

func (p *controlSocketPool) acquire(addr string) (net.Listener, error) {
	path := addr
	if i := strings.LastIndexByte(addr, '|'); i >= 0 {
		path = addr[:i]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}
	na, err := caddy.ParseNetworkAddress("unix/" + addr)
	if err != nil {
		return nil, fmt.Errorf("control socket %s: %w", addr, err)
	}
	lnAny, err := na.Listen(context.Background(), 0, net.ListenConfig{})
	if err != nil {
		return nil, err
	}
	ln, ok := lnAny.(net.Listener)
	if !ok {
		return nil, fmt.Errorf("control socket %s: %T is not a stream listener", addr, lnAny)
	}
	return ln, nil
}

func (p *controlSocketPool) closeAll() {}
