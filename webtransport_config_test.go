package janus

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

func parseGlobalWT(t *testing.T, input string) (*WebtransportSettings, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	d.Next()
	return parseWebtransportGlobal(d)
}

func parseRouteWT(t *testing.T, input string) (*WebtransportRoute, error) {
	t.Helper()
	d := caddyfile.NewTestDispenser(input)
	d.Next()
	return parseWebtransportRoute(d)
}

func TestWebtransportGlobalDefaults(t *testing.T) {
	ws, err := parseGlobalWT(t, "webtransport")
	if err != nil {
		t.Fatal(err)
	}
	if ws.maxSessions() != 256 || ws.maxDatagram() != 1200 || ws.idleTimeout() != wtDefaultIdleTimeout {
		t.Fatalf("defaults: %d %d %v", ws.maxSessions(), ws.maxDatagram(), ws.idleTimeout())
	}
	ws, err = parseGlobalWT(t, "webtransport {\n max_sessions 8\n max_datagram 1152\n idle_timeout 45s\n}")
	if err != nil {
		t.Fatal(err)
	}
	if ws.maxSessions() != 8 || ws.maxDatagram() != 1152 || ws.idleTimeout().Seconds() != 45 {
		t.Fatalf("options: %d %d %v", ws.maxSessions(), ws.maxDatagram(), ws.idleTimeout())
	}
}

func TestWebtransportGlobalRejects(t *testing.T) {
	cases := map[string]string{
		"webtransport /lyte":                                  "takes no arguments",
		"webtransport {\n listen :4433\n}":                    "unrecognized webtransport option",
		"webtransport {\n max_sessions 0\n}":                  "want 1 through 65535",
		"webtransport {\n max_sessions 70000\n}":              "want 1 through 65535",
		"webtransport {\n max_datagram 1201\n}":               "want 64 through 1200",
		"webtransport {\n max_datagram 63\n}":                 "want 64 through 1200",
		"webtransport {\n idle_timeout 29s\n}":                "at least 30s",
		"webtransport {\n idle_timeout soon\n}":               "want a duration",
		"webtransport {\n max_sessions 1\n max_sessions 2\n}": "duplicate option",
		"webtransport {\n max_sessions 1 2\n}":                "exactly one argument",
	}
	for input, want := range cases {
		_, err := parseGlobalWT(t, input)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", input, want, err)
		}
	}
}

func TestWebtransportRouteForms(t *testing.T) {
	r, err := parseRouteWT(t, "webtransport /lyte udp/127.0.0.1:41151")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "/lyte" || r.targetAddr().String() != "127.0.0.1:41151" || r.maxSessions() != 4 || len(r.Origin) != 0 {
		t.Fatalf("route: %+v", r)
	}
	r, err = parseRouteWT(t, "webtransport /v6 udp/[::1]:41151 {\n origin same viewer.example\n max_sessions 2\n}")
	if err != nil {
		t.Fatal(err)
	}
	if r.targetAddr().Addr().String() != "::1" || r.maxSessions() != 2 || len(r.Origin) != 2 {
		t.Fatalf("route: %+v", r)
	}
}

func TestWebtransportRouteRejects(t *testing.T) {
	cases := map[string]string{
		"webtransport":                                                        "want `webtransport <path> udp/<ip>:<port>`",
		"webtransport { }":                                                    "want `webtransport <path> udp/<ip>:<port>`",
		"webtransport /lyte":                                                  "want `webtransport <path> udp/<ip>:<port>`",
		"webtransport lyte udp/127.0.0.1:1":                                   "/-prefixed path",
		"webtransport /lyte/ udp/127.0.0.1:1":                                 "must be exact",
		"webtransport /a//b udp/127.0.0.1:1":                                  "must be exact",
		"webtransport /lyte?x udp/127.0.0.1:1":                                "must not contain",
		"webtransport /lyte 127.0.0.1:41151":                                  "want udp/<ip>:<port>",
		"webtransport /lyte udp/host.example:41151":                           "IP literal",
		"webtransport /lyte udp/::1:41151":                                    "IP literal",
		"webtransport /lyte udp/127.0.0.1:0":                                  "port must be nonzero",
		"webtransport /lyte udp/0.0.0.0:41151":                                "unspecified address",
		"webtransport /lyte udp/[::]:41151":                                   "unspecified address",
		"webtransport /lyte udp/224.0.0.251:5353":                             "multicast address",
		"webtransport /lyte udp/[ff02::fb]:5353":                              "multicast address",
		"webtransport /lyte udp/255.255.255.255:1":                            "broadcast address",
		"webtransport /lyte udp/[fe80::1%en0]:41151":                          "zone-scoped",
		"webtransport /lyte udp/127.0.0.1:1 {\n origin any\n}":                "not legal for a relay",
		"webtransport /lyte udp/127.0.0.1:1 {\n origin\n}":                    "want same or one or more hostnames",
		"webtransport /lyte udp/127.0.0.1:1 {\n max_sessions 0\n}":            "want 1 through 65535",
		"webtransport /lyte udp/127.0.0.1:1 {\n max_datagram 1200\n}":         "unrecognized webtransport route option",
		"webtransport /lyte udp/127.0.0.1:1 {\n origin same\n origin same\n}": "duplicate option",
	}
	for input, want := range cases {
		_, err := parseRouteWT(t, input)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want error containing %q, got %v", input, want, err)
		}
	}
}

func TestWebtransportOriginAllowed(t *testing.T) {
	same := []string(nil)
	if wtOriginAllowed(same, "", "pup.local") {
		t.Error("missing Origin must be refused")
	}
	if !wtOriginAllowed(same, "https://pup.local", "pup.local") || !wtOriginAllowed(same, "https://PUP.local:8443", "pup.local") {
		t.Error("same-host Origin must be allowed, scheme and port ignored")
	}
	if wtOriginAllowed(same, "https://evil.example", "pup.local") {
		t.Error("foreign Origin must be refused")
	}
	named := []string{"same", "viewer.example"}
	if !wtOriginAllowed(named, "https://viewer.example", "pup.local") || !wtOriginAllowed(named, "https://pup.local", "pup.local") {
		t.Error("named and same hosts must be allowed together")
	}
	if wtOriginAllowed([]string{"viewer.example"}, "https://pup.local", "pup.local") {
		t.Error("a named-only policy must not admit the request host")
	}
	if wtOriginAllowed(same, "not a url", "pup.local") || wtOriginAllowed(same, "null", "pup.local") {
		t.Error("an unparsable or opaque Origin must be refused")
	}
}
