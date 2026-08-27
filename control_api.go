package janus

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

type controlServer struct {
	mode     string
	server   *http.Server
	ln       net.Listener
	stopping atomic.Bool
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

// idempotentListener makes Close safe across Janus's early explicit close and
// net/http's documented deferred close in Server.Serve. This is required for
// Caddy's reference-counted listeners, where a second Close is not harmless.
type idempotentListener struct {
	net.Listener
	once sync.Once
	err  error
}

func (ln *idempotentListener) Close() error {
	ln.once.Do(func() { ln.err = ln.Listener.Close() })
	return ln.err
}

func (a *App) startControlListeners() error {
	for i := range a.Control {
		c := &a.Control[i]
		mux := a.controlMuxAt(c.basePath)
		handler := http.Handler(mux)
		if c.secret != "" {
			handler = bearerAuth(c.secret, mux)
		}
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		cs := &controlServer{mode: c.Mode, server: srv, conns: make(map[net.Conn]struct{})}
		srv.ConnState = func(conn net.Conn, state http.ConnState) {
			cs.mu.Lock()
			switch state {
			case http.StateNew:
				cs.conns[conn] = struct{}{}
			case http.StateClosed, http.StateHijacked:
				delete(cs.conns, conn)
			}
			cs.mu.Unlock()
		}
		if c.useTLS {
			// Defaults are the committed dev pair (see certs/README.md);
			// operators point cert:…/key:… at their own material.
			certFile := c.CertFile
			keyFile := c.KeyFile
			if certFile == "" {
				certFile = "certs/ripdev.io.crt"
				keyFile = "certs/ripdev.io.key"
			}
			cert, err := tls.LoadX509KeyPair(certFile, keyFile)
			if err != nil {
				return fmt.Errorf("control %s tls: %w (need %s / %s)", c.Mode, err, certFile, keyFile)
			}
			srv.TLSConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
		}

		if c.network == "unix" {
			if err := os.MkdirAll(filepath.Dir(c.addr), 0o755); err != nil {
				return fmt.Errorf("control internal: %w", err)
			}
		}

		// Bind through Caddy's listener API so sockets pool across config
		// swaps: on reload the new app shares the old app's socket instead
		// of failing to bind while the old app still holds it. Caddy also
		// unlinks unix sockets before binding and after the last close.
		na, err := caddy.ParseNetworkAddress(c.network + "/" + c.addr)
		if err != nil {
			return fmt.Errorf("control %s address %s: %w", c.Mode, c.Listen, err)
		}
		lnAny, err := na.Listen(a.ctx, 0, net.ListenConfig{})
		if err != nil {
			return fmt.Errorf("control %s listen %s: %w", c.Mode, c.Listen, err)
		}
		ln, ok := lnAny.(net.Listener)
		if !ok {
			return fmt.Errorf("control %s listen %s: %T is not a stream listener", c.Mode, c.Listen, lnAny)
		}
		if c.useTLS {
			ln = tls.NewListener(ln, srv.TLSConfig)
		}
		ln = &idempotentListener{Listener: ln}

		cs.ln = ln
		a.controlSrvs = append(a.controlSrvs, cs)
		a.logger.Info("janus control listening",
			zap.String("mode", c.Mode),
			zap.String("listen", c.Listen),
			zap.String("network", c.network),
			zap.String("addr", c.addr),
			zap.Bool("auth", c.secret != ""),
		)
		go func(s *controlServer) {
			err := s.server.Serve(s.ln)
			if err != nil && !errors.Is(err, http.ErrServerClosed) && !s.stopping.Load() {
				a.logger.Error("janus control server stopped",
					zap.String("mode", s.mode),
					zap.Error(err),
				)
			}
		}(cs)
	}
	return nil
}

func (s *controlServer) closeConnections() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (a *App) stopControlListeners() error {
	var wg sync.WaitGroup
	var first error
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Close our listener handles before Shutdown. A failed Start can unwind
	// immediately after launching Serve; closing here prevents that goroutine
	// from racing past Shutdown's listener bookkeeping and accepting afterward.
	// Caddy's pooled listener remains live when another generation shares it.
	for _, s := range a.controlSrvs {
		s.stopping.Store(true)
		if err := s.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) && first == nil {
			first = err
		}
	}
	// Shutdown now drains active connections. Caddy unlinks a unix socket only
	// when the last pooled listener closes, so an overlapping app keeps it live.
	for _, s := range a.controlSrvs {
		wg.Add(1)
		go func(s *controlServer) {
			defer wg.Done()
			if err := s.server.Shutdown(ctx); err != nil {
				// The drain deadline expired: a handler slower than the
				// deadline must not outlive its generation, so force the
				// remaining connections closed.
				s.closeConnections()
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	a.controlSrvs = nil
	return first
}

func (a *App) controlMux() *http.ServeMux {
	return a.controlMuxAt("")
}

func (a *App) controlMuxAt(base string) *http.ServeMux {
	mux := http.NewServeMux()
	p1 := base + "/1.0"
	p2 := base + "/1.0/health"
	// "/{$}" matches only the trailing-slash form — never a subtree.
	// An unknown path under /1.0 gets the mux's 404 and a known path
	// with the wrong method gets its 405; a typo'd or wrong-method
	// call must never get a 200 that masks the mistake.
	mux.HandleFunc(p1, a.handleControlRoot)
	mux.HandleFunc(p1+"/{$}", a.handleControlRoot)
	mux.HandleFunc(p2, a.handleControlHealth)
	mux.HandleFunc(p2+"/{$}", a.handleControlHealth)

	apps := base + "/1.0/apps"
	mux.HandleFunc("POST "+apps, a.handleAppsCreate)
	mux.HandleFunc("GET "+apps, a.handleAppsList)
	mux.HandleFunc("GET "+apps+"/{id}", a.handleAppsGet)
	mux.HandleFunc("PATCH "+apps+"/{id}", a.handleAppsPatch)
	mux.HandleFunc("DELETE "+apps+"/{id}", a.handleAppsDelete)
	mux.HandleFunc("PUT "+apps+"/{id}/upstreams", a.handleAppsUpstreamsPut)
	mux.HandleFunc("POST "+apps+"/{id}/heartbeat", a.handleAppsHeartbeat)
	mux.HandleFunc(apps+"/{id}/access", a.handleAccessStream)
	mux.HandleFunc("GET "+base+"/1.0/access", a.handleAccessStatus)
	mux.HandleFunc("GET "+base+"/1.0/access/{$}", a.handleAccessStatus)

	mux.HandleFunc("GET "+base+"/1.0/tls/ask", a.handleTLSAsk)

	// Cache counters, always on: a non-blocking snapshot of per-shard
	// atomics (monotonic, not mutually atomic). A tight scrape loop
	// can never degrade the data plane.
	mux.HandleFunc("GET "+base+"/1.0/cache", a.handleCacheStats)
	mux.HandleFunc("GET "+base+"/1.0/cache/{$}", a.handleCacheStats)

	// Hub: publish plane, membership snapshot, and counters (always on).
	mux.HandleFunc("POST "+apps+"/{id}/hub/publish", a.handleHubPublish)
	mux.HandleFunc("GET "+apps+"/{id}/hub", a.handleHubSnapshot)
	mux.HandleFunc("GET "+base+"/1.0/hub", a.handleHubStats)
	mux.HandleFunc("GET "+base+"/1.0/hub/{$}", a.handleHubStats)

	// mDNS advertiser state, always on: {"enabled": false} when the
	// capability is off, the full advertiser view when on.
	mux.HandleFunc("GET "+base+"/1.0/mdns", a.handleMdnsState)
	mux.HandleFunc("GET "+base+"/1.0/mdns/{$}", a.handleMdnsState)

	// Auth wall state, always on: {"enabled": false} when no site's
	// effective auth is on; counters, the session list, and
	// revocation (observe and revoke — never configure).
	mux.HandleFunc("GET "+base+"/1.0/auth", a.handleAuthState)
	mux.HandleFunc("GET "+base+"/1.0/auth/{$}", a.handleAuthState)
	mux.HandleFunc("GET "+base+"/1.0/auth/sessions", a.handleAuthSessions)
	mux.HandleFunc("DELETE "+base+"/1.0/auth/sessions", a.handleAuthSessionsWipe)
	mux.HandleFunc("DELETE "+base+"/1.0/auth/sessions/{id}", a.handleAuthSessionDelete)

	mux.HandleFunc("GET "+base+"/1.0/browse", a.handleBrowseState)
	mux.HandleFunc("GET "+base+"/1.0/browse/{$}", a.handleBrowseState)
	return mux
}

func (a *App) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.cache.snapshot())
}

// handleTLSAsk answers Caddy's on_demand_tls ask: may a certificate be
// minted for this domain? 200 = the domain is a host claimed by a
// registered app; 404 = it is not (Caddy denies on any non-200). The
// lookup uses the same normalized exact, alias, and directory-gated site
// resolution as HTTP. Allowance follows the registry lifecycle and the
// live direct-child gate: register → allowed; DELETE, TTL reap, or site
// directory removal → denied. Heartbeat ≠ readiness: an alive app with
// empty upstreams keeps its allowance — a reload never breaks TLS.
func (a *App) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeAPIError(w, errBadRequest("domain query parameter is required"))
		return
	}
	name := normalizeHostHeader(domain)
	rec, ok := a.appsRegistry().resolveRequestHost(domain)
	if !ok {
		if a.state != nil && a.state.browse.coldClaim(name) {
			writeJSON(w, http.StatusOK, map[string]string{"domain": name, "claim": "cold"})
			return
		}
		writeAPIError(w, &apiError{
			Status: http.StatusNotFound,
			Msg:    fmt.Sprintf("domain %q is not a host of any registered app", name),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"domain": name, "app": rec.ID})
}

func (a *App) handleControlRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "1.0",
		"type":        "janus",
		"ping":        cascadeBool(nil, a.Ping, false),
		"mdns":        a.Mdns != nil,
		"auth":        len(a.authEnabledSites()) > 0,
		"files":       cascadeBool(nil, a.Files, a.Browse != nil),
		"browse":      a.Browse != nil,
		"control":     a.controlPublicInfo(),
	})
}

func (a *App) handleBrowseState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.browseStatus())
}

func (a *App) handleControlHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
	})
}

func (a *App) controlPublicInfo() []map[string]any {
	out := make([]map[string]any, 0, len(a.Control))
	for _, c := range a.Control {
		out = append(out, map[string]any{
			"mode":   c.Mode,
			"listen": c.Listen,
			"auth":   c.secret != "",
		})
	}
	return out
}

func bearerAuth(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const p = "Bearer "
		ok := strings.HasPrefix(h, p) &&
			subtle.ConstantTimeCompare([]byte(h[len(p):]), []byte(secret)) == 1
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="janus"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
