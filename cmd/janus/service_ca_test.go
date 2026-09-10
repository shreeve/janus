package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestStatusUsesServiceStorageEnvironment(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	seedAt(t, p)
	data := t.TempDir()
	path := filepath.Join(data, "caddy", "pki", "authorities", "local", "root.crt")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testRootPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.env, []byte("XDG_DATA_HOME="+data+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	caller := os.Getenv("XDG_DATA_HOME")
	st := gatherStatus(p)
	if st.CA != path || st.CATrusted == nil {
		t.Fatalf("wrong service CA: %+v", st)
	}
	if os.Getenv("XDG_DATA_HOME") != caller {
		t.Fatal("status changed caller environment")
	}
	root := p
	root.root = true
	root.home = "/var/lib/janus"
	root.env = filepath.Join(t.TempDir(), "absent")
	if got := serviceCARoot(root); !filepath.IsAbs(got) || !bytes.HasPrefix([]byte(got), []byte(root.home+"/")) {
		t.Fatalf("root service uses caller storage: %s", got)
	}
}

func TestStatusVerifiesLiveCAInsteadOfStaleDiskCertificate(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	seedAt(t, p)
	controlServer(t, p, `[]`)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	path := serviceCARoot(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, testRootPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	live := testRootPEM(t)
	expected := parseRootCA(live)
	previous := verifyCA
	verifyCA = func(cert *x509.Certificate) error {
		if bytes.Equal(cert.Raw, expected.Raw) {
			return nil
		}
		return errors.New("wrong CA")
	}
	t.Cleanup(func() { verifyCA = previous })
	ln, err := net.Listen("unix", p.admin)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pki/ca/local" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"root_certificate": string(live)})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	st := gatherStatus(p)
	if st.CATrusted == nil || !*st.CATrusted || st.CA != "" {
		t.Fatalf("stale CA selected: %+v", st)
	}
}
