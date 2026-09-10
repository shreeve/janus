package janus

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

func TestHeartbeatTTLReloadMustMatchRunningRegistry(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()
	first := &App{HeartbeatTTL: caddy.Duration(15 * time.Second)}
	defer first.Cleanup()
	if err := first.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ttl := range []time.Duration{30 * time.Second, 15 * time.Second} {
		next := &App{HeartbeatTTL: caddy.Duration(ttl)}
		err := next.Provision(ctx)
		if ttl != 15*time.Second {
			if err == nil || !strings.Contains(err.Error(), "requires a restart") {
				t.Errorf("changed TTL: %v", err)
			}
		} else if err != nil || next.appsReg != first.appsReg {
			t.Errorf("same TTL failed to retain registry: %v", err)
		}
		if err := next.Cleanup(); err != nil {
			t.Fatal(err)
		}
	}
	if first.appsReg.ttl != 15*time.Second {
		t.Fatal("failed reload mutated running TTL")
	}
	w := httptest.NewRecorder()
	first.handleControlRoot(w, httptest.NewRequest("GET", "/1.0", nil))
	var meta struct {
		HeartbeatTTL string `json:"heartbeat_ttl"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.HeartbeatTTL != "15s" {
		t.Fatalf("effective TTL: %q", meta.HeartbeatTTL)
	}
}
