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
		if c.UseTLS {
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

		if c.Network == "unix" {
			if err := os.MkdirAll(filepath.Dir(c.Addr), 0o755); err != nil {
				return fmt.Errorf("control internal: %w", err)
			}
			_ = os.Remove(c.Addr)
		}

		ln, err := net.Listen(c.Network, c.Addr)
		if err != nil {
			return fmt.Errorf("control %s listen %s: %w", c.Mode, c.Listen, err)
		}
		if c.UseTLS {
			ln = tls.NewListener(ln, srv.TLSConfig)
		}

		cs := &controlServer{mode: c.Mode, server: srv, ln: ln}
		a.controlSrvs = append(a.controlSrvs, cs)
		a.logger.Info("janus control listening",
			zap.String("mode", c.Mode),
			zap.String("listen", c.Listen),
			zap.String("network", c.Network),
			zap.String("addr", c.Addr),
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
			if s.ln != nil && s.ln.Addr().Network() == "unix" {
				_ = os.Remove(s.ln.Addr().String())
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
		paths[c.BasePath] = true
	}
	for base := range paths {
		p1 := base + "/1.0"
		p2 := base + "/1.0/health"
		mux.HandleFunc(p1, a.handleControlRoot)
		mux.HandleFunc(p1+"/", a.handleControlRoot)
		mux.HandleFunc(p2, a.handleControlHealth)
		mux.HandleFunc(p2+"/", a.handleControlHealth)

		apps := base + "/1.0/apps"
		mux.HandleFunc("POST "+apps, a.handleAppsCreate)
		mux.HandleFunc("GET "+apps, a.handleAppsList)
		mux.HandleFunc("GET "+apps+"/{id}", a.handleAppsGet)
		mux.HandleFunc("PATCH "+apps+"/{id}", a.handleAppsPatch)
		mux.HandleFunc("DELETE "+apps+"/{id}", a.handleAppsDelete)
		mux.HandleFunc("PUT "+apps+"/{id}/upstreams", a.handleAppsUpstreamsPut)
		mux.HandleFunc("POST "+apps+"/{id}/heartbeat", a.handleAppsHeartbeat)
	}
	return mux
}

func (a *App) handleControlRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_version": "1.0",
		"type":        "janus",
		"ping":        resolveBool(nil, a.Ping, false),
		"control":     a.controlPublicInfo(),
	})
}

func (a *App) handleControlHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
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
