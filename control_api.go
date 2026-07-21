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
	"time"

	"github.com/caddyserver/caddy/v2"
	"go.uber.org/zap"
)

type controlServer struct {
	mode   string
	server *http.Server
	ln     net.Listener
}

func (a *App) startControlListeners() error {
	mux := a.controlMux()
	for i := range a.Control {
		c := &a.Control[i]
		handler := http.Handler(mux)
		if c.secret != "" {
			handler = bearerAuth(c.secret, mux)
		}
		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		if c.useTLS {
			certFile := "certs/ripdev.io.crt"
			keyFile := "certs/ripdev.io.key"
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

		cs := &controlServer{mode: c.Mode, server: srv, ln: ln}
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
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.logger.Error("janus control server stopped",
					zap.String("mode", s.mode),
					zap.Error(err),
				)
			}
		}(cs)
	}
	return nil
}

func (a *App) stopControlListeners() error {
	var wg sync.WaitGroup
	var first error
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Shutdown closes each pooled listener; Caddy unlinks a unix socket
	// only when its last user closes, so a reload's overlapping app keeps
	// the socket file.
	for _, s := range a.controlSrvs {
		wg.Add(1)
		go func(s *controlServer) {
			defer wg.Done()
			if err := s.server.Shutdown(ctx); err != nil {
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
	mux := http.NewServeMux()
	// Register under each base path; empty base → /1.0
	paths := map[string]bool{"": true}
	for _, c := range a.Control {
		paths[c.basePath] = true
	}
	for base := range paths {
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
	}
	return mux
}

func (a *App) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.cache.snapshot())
}

// handleTLSAsk answers Caddy's on_demand_tls ask: may a certificate be
// minted for this domain? 200 = the domain is a host claimed by a
// registered app; 404 = it is not (Caddy denies on any non-200). The
// match is exact after hostname normalization (lowercase, trailing dot
// stripped) — the registry holds exact hosts only, never wildcards.
// Allowance follows the registry lifecycle: register → allowed; DELETE
// or TTL reap → denied. Heartbeat ≠ readiness: an alive app with empty
// upstreams keeps its allowance — a reload never breaks TLS.
func (a *App) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		writeAPIError(w, errBadRequest("domain query parameter is required"))
		return
	}
	name := normalizeHostHeader(domain)
	rec, ok := a.appsRegistry().resolveHost(name)
	if !ok {
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
		"control":     a.controlPublicInfo(),
	})
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
