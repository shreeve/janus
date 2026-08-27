//go:build unix && !solaris

package janus

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// controlSocketPool owns every unix control socket for the whole process.
//
// Caddy's unix listener reuse on these platforms (listen_unix.go) keys a
// package map by path and repoints the entry at the NEWEST generation's
// listener on every reuse. A generation that aborts — bound its sockets,
// then a later start step failed — closes that newest listener, and the
// map is left holding a corpse: every later Listen for the path dials
// File() on a closed listener and fails with "use of closed network
// connection" until the process restarts. One bad reload wedges every
// reload after it.
//
// The pool fixes that by construction. It owns one CANONICAL listener per
// path that no generation ever serves on or closes; each generation
// acquires a dup'd descriptor (File → net.FileListener, the same dup
// dance caddy itself uses for overlap) and closes only its dup. Dup'd
// descriptors share one accept queue, so overlapping generations behave
// exactly as before — and an aborted generation's close touches nothing
// but its own dup. Teardown is symmetric: no abort-versus-retirement
// detection exists because none is needed. The last release closes the
// canonical, whose unlink-on-close removes the socket file.
type controlSocketPool struct {
	mu      sync.Mutex
	entries map[string]*controlSocketEntry
}

type controlSocketEntry struct {
	canonical *net.UnixListener
	refs      int
}

// splitSocketPerms splits caddy's "path|permissions" unix address form.
// The default matches caddy's: 0o200, owner-writable only — connecting to
// a socket needs write, nothing needs to read the file itself.
func splitSocketPerms(addr string) (string, fs.FileMode, error) {
	path, mode := addr, fs.FileMode(0o200)
	if i := strings.LastIndexByte(addr, '|'); i >= 0 {
		bits := addr[i+1:]
		path = addr[:i]
		n, err := strconv.ParseUint(bits, 8, 32)
		if err != nil || n > 0o777 {
			return "", 0, fmt.Errorf("invalid unix socket permissions %q", bits)
		}
		mode = fs.FileMode(n)
	}
	// Connecting to a unix socket needs write permission; a mode without
	// owner-write binds fine and then locks every client out with EACCES.
	// Fail loudly at listen time instead (caddy enforces the same).
	if mode&0o200 == 0 {
		return "", 0, fmt.Errorf("unix socket permissions %04o deny the owner write ('-w-') — no client could connect", uint32(mode))
	}
	if path == "" {
		return "", 0, errors.New("unix socket path is empty")
	}
	return path, mode, nil
}

// acquire returns a listener for the unix socket at addr. The first
// acquire for a path binds the canonical (removing a stale file left by
// an unclean previous process); every acquire returns a fresh dup whose
// Close releases exactly one reference.
func (p *controlSocketPool) acquire(addr string) (net.Listener, error) {
	path, mode, err := splitSocketPerms(addr)
	if err != nil {
		return nil, err
	}
	abstract := strings.HasPrefix(path, "@")

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]*controlSocketEntry)
	}
	e := p.entries[path]
	created := false
	if e == nil {
		if !abstract {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, fmt.Errorf("control socket %s: %w", path, err)
			}
			// A previous process that died without cleanup leaves its
			// socket file behind; a live file cannot be rebound.
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("control socket %s: removing stale file: %w", path, err)
			}
		}
		ln, err := net.Listen("unix", path)
		if err != nil {
			return nil, err
		}
		// net created this file, so unlink-on-close is already on: the
		// canonical's close (last release, or pool teardown) removes it.
		e = &controlSocketEntry{canonical: ln.(*net.UnixListener)}
		p.entries[path] = e
		created = true
	}
	// Chmod on EVERY acquire, not just the first bind: a reload that
	// changes the |bits of a live socket must take effect now, not at
	// the next process restart (caddy re-chmods on reuse the same way).
	if !abstract {
		if err := os.Chmod(path, mode); err != nil {
			if created {
				_ = e.canonical.Close()
				delete(p.entries, path)
			}
			return nil, fmt.Errorf("control socket %s: %w", path, err)
		}
	}

	dup, err := dupUnixListener(e.canonical)
	if err != nil {
		if e.refs == 0 {
			_ = e.canonical.Close()
			delete(p.entries, path)
		}
		return nil, fmt.Errorf("control socket %s: %w", path, err)
	}
	e.refs++
	return &pooledControlListener{Listener: dup, pool: p, path: path}, nil
}

// dupUnixListener copies the canonical's descriptor into an independent
// listener. FileListener's socket was not created by package net, so its
// close never unlinks the file — only the canonical's does.
func dupUnixListener(canonical *net.UnixListener) (net.Listener, error) {
	f, err := canonical.File() // dup(2)
	if err != nil {
		return nil, err
	}
	// FileListener dups again; the intermediate copy must not leak.
	defer f.Close()
	return net.FileListener(f)
}

// pooledControlListener releases its pool reference exactly once, however
// many times net/http's layered deferred closes fire.
type pooledControlListener struct {
	net.Listener
	pool *controlSocketPool
	path string
	once sync.Once
}

func (l *pooledControlListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { l.pool.release(l.path) })
	return err
}

func (p *controlSocketPool) release(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[path]
	if e == nil {
		return
	}
	if e.refs--; e.refs <= 0 {
		_ = e.canonical.Close()
		delete(p.entries, path)
	}
}

// closeAll is Destruct's safety net. Normally the map is already empty:
// every generation's Cleanup closes its dup before the pooled state is
// released. Nil-safe so a hand-built janusState cannot panic teardown.
func (p *controlSocketPool) closeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for path, e := range p.entries {
		_ = e.canonical.Close()
		delete(p.entries, path)
	}
}
