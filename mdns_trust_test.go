package janus

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func testRootCert(t *testing.T, cn string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// The trust routes ride the front door: over plain HTTP on the shared
// port for every front-door host, and over TLS for the door's own names
// (the status page there, and /trust/check as the handshake the trust
// page probes). An app's .local host over TLS is the app's.
func TestTrustFrontDoor(t *testing.T) {
	app := newTestSharedMdnsApp(t)
	root := testRootCert(t, "Caddy Local Authority - 2026 ECC Root")
	app.rootCAFor = func() (*x509.Certificate, error) { return root, nil }
	h := &Handler{app: app, dp: app.dp, logger: zap.NewNop()}
	serve := func(path, host string, secure bool) (*httptest.ResponseRecorder, bool, error) {
		req := httptest.NewRequest("GET", "http://placeholder"+path, nil)
		req.Host = host
		req = req.WithContext(context.WithValue(req.Context(),
			http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4zero, Port: map[bool]int{false: 80, true: 443}[secure]}))
		if secure {
			req.TLS = &tls.ConnectionState{}
		}
		nextCalled := false
		next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			nextCalled = true
			w.WriteHeader(http.StatusTeapot)
			return nil
		})
		rr := httptest.NewRecorder()
		err := h.ServeHTTP(rr, req, next)
		return rr, nextCalled, err
	}

	for _, tc := range []struct {
		path, host string
		secure     bool
		want       int
		body       string
	}{
		{"/trust", "janus.local", false, 200, "One-time setup"},
		{"/trust/", "janus.local", false, 200, "One-time setup"},
		{"/trust/safari", "janus.local", false, 200, "/trust/ca.mobileconfig"},
		{"/trust/check", "janus.local", false, 204, ""},
		{"/trust/ca.crt", "janus.local", false, 200, "-----BEGIN CERTIFICATE-----"},
		{"/trust/ca.mobileconfig", "janus.local", false, 200, "com.apple.security.root"},
		{"/trust", "shop.local", false, 200, "One-time setup"}, // any front-door host on the shared port
		{"/", "janus.local", true, 200, "status"},
		{"/trust/check", "janus.local", true, 204, ""},
		{"/trust", "janus-2.local", true, 200, "One-time setup"},       // effective name
		{"/trust", "janus.lan.ripdev.io", true, 200, "One-time setup"}, // canonical
	} {
		rr, nextCalled, err := serve(tc.path, tc.host, tc.secure)
		if err != nil || nextCalled {
			t.Errorf("%s (Host %s, tls %v): err %v, next %v", tc.path, tc.host, tc.secure, err, nextCalled)
			continue
		}
		if rr.Code != tc.want || !strings.Contains(rr.Body.String(), tc.body) {
			t.Errorf("%s (Host %s, tls %v) = %d %q, want %d containing %q", tc.path, tc.host, tc.secure, rr.Code, rr.Body.String(), tc.want, tc.body)
		}
	}
	rr, _, _ := serve("/trust/ca.crt", "janus.local", false)
	block, _ := pem.Decode(rr.Body.Bytes())
	if block == nil || !strings.Contains(rr.Header().Get("Content-Disposition"), "janus-local-ca.crt") {
		t.Fatalf("ca.crt: %q %q", rr.Header().Get("Content-Disposition"), rr.Body.String())
	}
	if got, _ := x509.ParseCertificate(block.Bytes); got == nil || !got.Equal(root) {
		t.Error("ca.crt is not the root")
	}
	rr, _, _ = serve("/trust/check", "janus.local", false)
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" || rr.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("check headers: %v", rr.Header())
	}

	// Over TLS, an app's host is the app's, not the door's: the data
	// plane serves it (unknown app socket → its 404), next is never consulted.
	rr, nextCalled, err := serve("/trust", "shop.local", true)
	var herr caddyhttp.HandlerError
	if !errors.As(err, &herr) || herr.StatusCode != 404 || nextCalled {
		t.Errorf("shop.local over TLS: err %v code %d next %v", err, rr.Code, nextCalled)
	}

	// No CA to hand out: the certificate routes say so, the page still serves.
	app.rootCAFor = func() (*x509.Certificate, error) { return nil, errors.New("no pki") }
	for _, path := range []string{"/trust/ca.crt", "/trust/ca.mobileconfig"} {
		if rr, _, _ := serve(path, "janus.local", false); rr.Code != 404 {
			t.Errorf("%s without a CA = %d", path, rr.Code)
		}
	}
	if rr, _, _ := serve("/trust", "janus.local", false); rr.Code != 200 {
		t.Errorf("/trust without a CA = %d", rr.Code)
	}
}

func TestMdnsFrontDoorName(t *testing.T) {
	app := newTestSharedMdnsApp(t)
	for host, want := range map[string]bool{
		"janus.local": true, "janus-2.local": true, "janus.lan.ripdev.io": true,
		"shop.local": false, "other.local": false, "127.0.0.1": false,
	} {
		if got := app.mdnsFrontDoorName(host); got != want {
			t.Errorf("frontDoorName(%q) = %v, want %v", host, got, want)
		}
	}
	if (&App{}).mdnsFrontDoorName("janus.local") {
		t.Error("no mdns, yet a front-door name")
	}
	// Configured but not yet started: the configured name is the door's.
	cold := &App{Mdns: &MdnsSettings{Name: "janus.local"}}
	if !cold.mdnsFrontDoorName("janus.local") || cold.mdnsFrontDoorName("janus-2.local") {
		t.Error("cold front-door name")
	}
}

// The iOS profile: the root as base64 DER, the certificate's common name
// on the payload, identifiers fixed, UUIDs a function of the certificate
// so a reinstall replaces rather than stacks.
func TestTrustProfile(t *testing.T) {
	root := testRootCert(t, "Caddy Local Authority & <Co>")
	got := string(trustProfile(root.Raw, root.Subject.CommonName))
	for _, want := range []string{
		"<data>" + base64.StdEncoding.EncodeToString(root.Raw) + "</data>",
		"<string>Caddy Local Authority &amp; &lt;Co&gt;</string>",
		"<string>com.apple.security.root</string>",
		"<string>janus.edge.ca</string>", "<string>janus.edge.trust</string>",
		"<string>Janus local HTTPS</string>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile lacks %q", want)
		}
	}
	uuids := regexp.MustCompile(`<key>PayloadUUID</key>\s*<string>([^<]+)</string>`).FindAllStringSubmatch(got, -1)
	if len(uuids) != 2 || uuids[0][1] == uuids[1][1] {
		t.Fatalf("uuids: %v", uuids)
	}
	shape := regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$`)
	for _, m := range uuids {
		if !shape.MatchString(m[1]) {
			t.Errorf("uuid %q", m[1])
		}
	}
	if again := string(trustProfile(root.Raw, root.Subject.CommonName)); again != got {
		t.Error("profile is not deterministic")
	}
	if other := string(trustProfile(testRootCert(t, "x").Raw, "x")); strings.Contains(other, uuids[0][1]) {
		t.Error("a different root reused a UUID")
	}
}

// Both trust pages are self-contained (a device that trusts nothing yet
// loads them) and render dynamic values as text nodes.
func TestTrustPagesSelfContained(t *testing.T) {
	for name, page := range map[string]string{"trust.html": string(trustPageHTML), "trust_safari.html": string(trustSafariHTML)} {
		if strings.Contains(page, "innerHTML") {
			t.Errorf("%s uses innerHTML", name)
		}
		for _, external := range []string{"<link", "script src", "https://cdn", "@import"} {
			if strings.Contains(page, external) {
				t.Errorf("%s references an external resource (%q)", name, external)
			}
		}
	}
	for _, required := range []string{
		"/trust/check", "no-cors", "location.replace", "textContent",
		"/trust/ca.mobileconfig", "/trust/ca.crt", "x-safari-https://", "/trust/safari",
		"Certificate Trust Settings", "janus trust", "CriOS|FxiOS|EdgiOS|GSA|OPT",
		"min-height: 44px", "@media (max-width: 600px)",
	} {
		if !strings.Contains(string(trustPageHTML), required) {
			t.Errorf("trust.html is missing %q", required)
		}
	}
	for _, required := range []string{"/trust/ca.mobileconfig", "Certificate Trust Settings", "Janus local HTTPS"} {
		if !strings.Contains(string(trustSafariHTML), required) {
			t.Errorf("trust_safari.html is missing %q", required)
		}
	}
}
