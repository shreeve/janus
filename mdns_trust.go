package janus

// Trust onboarding on the mdns front door: the page that walks a device
// through trusting this edge's local certificate authority, the CA
// itself as a certificate and as an iOS configuration profile, and the
// probe an untrusted device uses to notice when it has become trusted.
// Plain HTTP carries it, because a device that does not trust the CA yet
// cannot load anything over HTTPS from this edge; that is the whole
// reason the front door exists on port 80.

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddypki"
)

//go:embed trust.html
var trustPageHTML []byte

//go:embed trust_safari.html
var trustSafariHTML []byte

// rootCA is the local CA's root certificate: Caddy's default internal CA,
// the one `tls { issuer internal }` signs with. The pki app owns it.
func (a *App) rootCA() (*x509.Certificate, error) {
	if a.rootCAFor != nil {
		return a.rootCAFor()
	}
	pkiI, err := a.ctx.App("pki")
	if err != nil {
		return nil, fmt.Errorf("local CA: %w", err)
	}
	pki, ok := pkiI.(*caddypki.PKI)
	if !ok {
		return nil, fmt.Errorf("local CA: pki app is %T", pkiI)
	}
	ca, err := pki.GetCA(a.ctx, caddypki.DefaultCAID)
	if err != nil {
		return nil, fmt.Errorf("local CA: %w", err)
	}
	if ca == nil || ca.RootCertificate() == nil {
		return nil, errors.New("local CA: no root certificate")
	}
	return ca.RootCertificate(), nil
}

// mdnsFrontDoorName reports whether host is the front door's own name:
// the configured name, the effective (post-conflict) name, or the
// canonical hostname. An app's .local host is the app's, not the door's.
func (a *App) mdnsFrontDoorName(host string) bool {
	ms := a.Mdns
	if ms == nil {
		return false
	}
	if host == ms.Name || (ms.canonicalHost != "" && host == ms.canonicalHost) {
		return true
	}
	return a.state != nil && host == a.state.mdns.effectiveName(ms.Name)
}

func (a *App) trustRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /trust", a.trustServePage)
	mux.HandleFunc("GET /trust/{$}", a.trustServePage)
	mux.HandleFunc("GET /trust/safari", a.trustServeSafari)
	mux.HandleFunc("GET /trust/check", a.trustServeCheck)
	mux.HandleFunc("GET /trust/ca.crt", a.trustServeCert)
	mux.HandleFunc("GET /trust/ca.mobileconfig", a.trustServeProfile)
}

func (a *App) trustServePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// A public certificate on the canonical name proves nothing about
	// this device's trust in the local CA. Only local names can probe it.
	canCheck := strings.HasSuffix(normalizeHostHeader(r.Host), ".local")
	_, _ = w.Write(bytes.ReplaceAll(trustPageHTML, []byte("{{CAN_CHECK}}"), []byte(strconv.FormatBool(canCheck))))
}

func (a *App) trustServeSafari(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(trustSafariHTML)
}

// trustServeCheck is the probe target: the trust page fetches it over
// HTTPS from plain HTTP, and surviving the handshake is the signal. The
// body is nothing; the status is beside the point.
func (a *App) trustServeCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) trustServeCert(w http.ResponseWriter, r *http.Request) {
	root, err := a.rootCA()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-local-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})
}

func (a *App) trustServeProfile(w http.ResponseWriter, r *http.Request) {
	root, err := a.rootCA()
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	w.Header().Set("Content-Disposition", `attachment; filename="janus-local-edge.mobileconfig"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(trustProfile(root.Raw, root.Subject.CommonName))
}

// trustProfile wraps the root certificate in an iOS configuration
// profile. iOS installs CA trust only through a profile; a bare .crt
// saved from a browser is a dead file in the Files app. The payload is
// the DER, base64; the UUIDs derive from the certificate's digest so
// reinstalling replaces the profile instead of stacking duplicates.
func trustProfile(rootDER []byte, commonName string) []byte {
	sum := sha256.Sum256(rootDER)
	digest := hex.EncodeToString(sum[:])
	uuidAt := func(off int) string {
		s := digest[off : off+32]
		return strings.ToUpper(s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32])
	}
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
		<dict>
			<key>PayloadCertificateFileName</key>
			<string>janus-local-ca.crt</string>
			<key>PayloadContent</key>
			<data>%s</data>
			<key>PayloadDescription</key>
			<string>Trusts this edge's local certificate authority (%s) so its https names verify on this device.</string>
			<key>PayloadDisplayName</key>
			<string>%s</string>
			<key>PayloadIdentifier</key>
			<string>janus.edge.ca</string>
			<key>PayloadType</key>
			<string>com.apple.security.root</string>
			<key>PayloadUUID</key>
			<string>%s</string>
			<key>PayloadVersion</key>
			<integer>1</integer>
		</dict>
	</array>
	<key>PayloadDisplayName</key>
	<string>Janus local HTTPS</string>
	<key>PayloadIdentifier</key>
	<string>janus.edge.trust</string>
	<key>PayloadRemovalDisallowed</key>
	<false/>
	<key>PayloadType</key>
	<string>Configuration</string>
	<key>PayloadUUID</key>
	<string>%s</string>
	<key>PayloadVersion</key>
	<integer>1</integer>
</dict>
</plist>
`, base64.StdEncoding.EncodeToString(rootDER), xmlText(commonName), xmlText(commonName), uuidAt(0), uuidAt(32)))
}

func xmlText(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
