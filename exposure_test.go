package janus

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// In wan mode the local name families are refused at the handler (front
// door included), refused a certificate, and withdrawn from the air; a
// public name is untouched, and leaving wan mode brings the names back.
func TestWanExposureRefusesLocalNames(t *testing.T) {
	SetExposureScope("wan")
	t.Cleanup(func() { SetExposureScope("") })
	app := newTestSharedMdnsApp(t)
	if _, err := app.appsReg.create("shop", []string{"shop.local", "shop.example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	h := &Handler{app: app, dp: app.dp, logger: zap.NewNop()}
	serve := func(host string, secure bool) (int, bool, error) {
		req := httptest.NewRequest("GET", "http://placeholder/", nil)
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
		return rr.Code, nextCalled, err
	}
	for _, host := range []string{"janus.local", "janus-2.local", "shop.local", "shop.localhost", "localhost", "via.rip", "shop.via.rip"} {
		for _, secure := range []bool{false, true} {
			_, nextCalled, err := serve(host, secure)
			var herr caddyhttp.HandlerError
			if !errors.As(err, &herr) || herr.StatusCode != http.StatusMisdirectedRequest || nextCalled {
				t.Errorf("%s (tls %v): err %v next %v, want 421 and no pass-through", host, secure, err, nextCalled)
			}
		}
	}
	// A public name on the shared HTTP port still passes through to the
	// redirect, as it always does.
	if code, nextCalled, err := serve("shop.example.com", false); err != nil || !nextCalled || code != http.StatusTeapot {
		t.Errorf("shop.example.com: code %d next %v err %v", code, nextCalled, err)
	}

	if _, err := app.certificateAllowed("shop.local"); err == nil {
		t.Error("a registered .local name was certified in wan mode")
	}
	if _, err := app.certificateAllowed("janus.local"); err == nil {
		t.Error("the front door's name was certified in wan mode")
	}
	if _, err := app.certificateAllowed("shop.example.com"); err != nil {
		t.Errorf("a registered public name was refused in wan mode: %v", err)
	}

	rr := httptest.NewRecorder()
	app.handleMdnsState(rr, httptest.NewRequest("GET", "/1.0/mdns", nil))
	if !strings.Contains(rr.Body.String(), `"exposure":"wan"`) {
		t.Errorf("/1.0/mdns does not say wan:\n%s", rr.Body.String())
	}

	SetExposureScope("lan")
	if _, err := app.certificateAllowed("shop.local"); err != nil {
		t.Errorf("back in lan mode, a registered .local name was refused: %v", err)
	}
	// The data plane takes it again, whatever it answers; never the 421.
	if _, _, err := serve("shop.local", true); err != nil {
		var herr caddyhttp.HandlerError
		if errors.As(err, &herr) && herr.StatusCode == http.StatusMisdirectedRequest {
			t.Error("back in lan mode, shop.local was still refused")
		}
	}
}

// The advertiser carries nothing in wan mode and re-announces when the
// mode returns, on its own reconcile cadence.
func TestWanExposureWithdrawsAdvertising(t *testing.T) {
	t.Cleanup(func() { SetExposureScope("") })
	reg := newAppRegistry()
	if _, err := reg.create("shop", []string{"shop.local"}, ""); err != nil {
		t.Fatal(err)
	}
	fake := &fakeResponder{}
	adv := newTestAdvertiser(t, reg, fake)
	if err := adv.configure(t, &mdnsConfig{name: "janus.local", port: 80, apps: true}); err != nil {
		t.Fatal(err)
	}
	adv.reconcile()
	if n := len(adv.snapshot("janus.local").entries); n != 2 {
		t.Fatalf("entries before wan = %d, want 2", n)
	}
	SetExposureScope("wan")
	adv.reconcile()
	if n := len(adv.snapshot("janus.local").entries); n != 0 {
		t.Fatalf("entries in wan = %d, want 0", n)
	}
	if adv.withdraws.Load() != 2 {
		t.Errorf("withdraws = %d, want 2 (goodbyes for both names)", adv.withdraws.Load())
	}
	SetExposureScope("lan")
	adv.reconcile()
	if n := len(adv.snapshot("janus.local").entries); n != 2 {
		t.Fatalf("entries after wan = %d, want 2", n)
	}
}

func TestLocalFamilyHost(t *testing.T) {
	for host, want := range map[string]bool{
		"janus.local": true, "shop.localhost": true, "localhost": true, "a.b.local": true, "via.rip": true, "shop.via.rip": true,
		"shop.example.com": false, "local": false, "localhost.example.com": false, "127.0.0.1": false, "via.rip.example.com": false,
	} {
		if got := localFamilyHost(host); got != want {
			t.Errorf("localFamilyHost(%q) = %v, want %v", host, got, want)
		}
	}
}
