package main

import "testing"

func TestLoopbackAddr(t *testing.T) {
	for in, want := range map[string]string{
		"files.janus.localhost:443":  "127.0.0.1:443",
		"WT.Janus.Localhost.:8443":   "127.0.0.1:8443",
		"localhost:443":              "127.0.0.1:443",
		"example.com:443":            "example.com:443",
		"notlocalhost:443":           "notlocalhost:443",
		"files.janus.localhost":      "files.janus.localhost",
		"[::1]:443":                  "[::1]:443",
		"evil.localhost.example:443": "evil.localhost.example:443",
	} {
		if got := loopbackAddr(in); got != want {
			t.Errorf("loopbackAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
