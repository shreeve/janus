package janus

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func TestParseHeartbeatTTL(t *testing.T) {
	d := caddyfile.NewTestDispenser("janus {\n heartbeat_ttl 30s \n}")
	app := new(App)
	if err := app.UnmarshalCaddyfile(d); err != nil {
		t.Fatal(err)
	}
	if time.Duration(app.HeartbeatTTL) != 30*time.Second {
		t.Fatalf("heartbeat_ttl: got %v", caddy.Duration(app.HeartbeatTTL))
	}
	for _, bad := range []string{
		"janus {\n heartbeat_ttl \n}",
		"janus {\n heartbeat_ttl abc \n}",
		"janus {\n heartbeat_ttl 0s \n}",
		"janus {\n heartbeat_ttl -5s \n}",
		"janus {\n heartbeat_ttl 5s \n heartbeat_ttl 6s \n}",
	} {
		d := caddyfile.NewTestDispenser(bad)
		if err := new(App).UnmarshalCaddyfile(d); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCascadeBool(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name    string
		site    *bool
		global  *bool
		builtin bool
		want    bool
	}{
		{"all unset → builtin off", nil, nil, false, false},
		{"all unset → builtin on", nil, nil, true, true},
		{"global on, site unset", nil, &on, false, true},
		{"global off, site unset", nil, &off, true, false},
		{"site off overrides global on", &off, &on, false, false},
		{"site on overrides global off", &on, &off, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cascadeBool(tt.site, tt.global, tt.builtin); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseOnOff(t *testing.T) {
	tests := []struct {
		args    []string
		want    bool
		wantErr bool
	}{
		{nil, true, false},
		{[]string{}, true, false},
		{[]string{"on"}, true, false},
		{[]string{"off"}, false, false},
		{[]string{"maybe"}, false, true},
		{[]string{"on", "off"}, false, true},
	}
	for _, tt := range tests {
		got, err := parseOnOff(tt.args)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("args %v: want error", tt.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("args %v: %v", tt.args, err)
		}
		if got != tt.want {
			t.Fatalf("args %v: got %v, want %v", tt.args, got, tt.want)
		}
	}
}
