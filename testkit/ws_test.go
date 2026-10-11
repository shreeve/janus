package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// edgeWithCA serves a WebSocket upgrade at /hub over TLS with a test CA
// and returns the CA as a PEM file; the hub drivers dial it as the edge.
func edgeWithCA(t *testing.T) string {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := up.Upgrade(w, r, nil); err == nil {
			_ = c.Close()
		}
	}))
	t.Cleanup(srv.Close)
	prev := edgeAddr
	edgeAddr = srv.Listener.Addr().String()
	t.Cleanup(func() { edgeAddr = prev })
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// httptest's certificate names example.com; the drivers send the host as SNI
// and verify against it.
const edgeHost = "example.com"

func TestDialHubVerifiesTheEdge(t *testing.T) {
	ca := edgeWithCA(t)
	t.Setenv("SSL_CERT_FILE", "")

	// No bundle: the system roots do not trust the test CA, so the dial
	// fails instead of skipping verification.
	if c, _, err := dialHub(edgeHost, "-", "-", nil, 2*time.Second); err == nil {
		_ = c.Close()
		t.Fatal("an untrusted edge certificate was accepted")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("want a certificate error, got %v", err)
	}

	// The -cacert bundle trusts it.
	roots, err := trustRoots(ca)
	if err != nil {
		t.Fatal(err)
	}
	c, _, err := dialHub(edgeHost, "-", "-", roots, 2*time.Second)
	if err != nil {
		t.Fatalf("dial with -cacert: %v", err)
	}
	_ = c.Close()

	// So does SSL_CERT_FILE when no flag is given.
	t.Setenv("SSL_CERT_FILE", ca)
	if roots, err = trustRoots(""); err != nil || roots == nil {
		t.Fatalf("SSL_CERT_FILE: %v", err)
	}
	c, _, err = dialHub(edgeHost, "-", "-", roots, 2*time.Second)
	if err != nil {
		t.Fatalf("dial with SSL_CERT_FILE: %v", err)
	}
	_ = c.Close()
}

func TestTrustRoots(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "")
	if roots, err := trustRoots(""); roots != nil || err != nil {
		t.Fatalf("no flag, no env: want the system roots (nil), got %v %v", roots, err)
	}
	if _, err := trustRoots(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("a missing bundle must be an error")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trustRoots(empty); err == nil {
		t.Fatal("a bundle with no certificate must be an error")
	}
	// The flag wins over the environment.
	t.Setenv("SSL_CERT_FILE", empty)
	ca := edgeWithCA(t)
	if roots, err := trustRoots(ca); err != nil || roots == nil {
		t.Fatalf("flag over env: %v", err)
	}
}

func TestCacertFlag(t *testing.T) {
	for _, tc := range []struct {
		args []string
		file string
		rest []string
	}{
		{[]string{"h", "-", "-", "close"}, "", []string{"h", "-", "-", "close"}},
		{[]string{"-cacert", "ca.pem", "h", "-", "-"}, "ca.pem", []string{"h", "-", "-"}},
		{[]string{"--cacert", "ca.pem", "h", "-", "-"}, "ca.pem", []string{"h", "-", "-"}},
		{[]string{"-cacert"}, "", []string{"-cacert"}},
	} {
		file, rest := cacertFlag(tc.args)
		if file != tc.file || strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("cacertFlag(%q) = %q %q", tc.args, file, rest)
		}
	}
}
