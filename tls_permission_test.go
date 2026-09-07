package janus

import (
	"context"
	"errors"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// The in-process permission decides by the same rule as the ask URL: a
// registered host mints, anything else is denied, and a config with no
// janus app denies everything rather than minting unchecked.
func TestPermissionByJanus(t *testing.T) {
	app := &App{}
	app.appsReg = newAppRegistry()
	if _, err := app.appsReg.create("shop", []string{"shop.local"}, "/rt/bridge"); err != nil {
		t.Fatal(err)
	}
	p := &PermissionByJanus{app: func() (*App, error) { return app, nil }}
	if err := p.CertificateAllowed(context.Background(), "shop.local"); err != nil {
		t.Errorf("registered host denied: %v", err)
	}
	if err := p.CertificateAllowed(context.Background(), "nobody.local"); err == nil {
		t.Error("unregistered host allowed")
	}
	// The mdns front door's own name is admitted too: its status page
	// and /trust/check answer over HTTPS.
	if err := p.CertificateAllowed(context.Background(), "janus.local"); err == nil {
		t.Error("front-door name minted with mdns off")
	}
	app.Mdns = &MdnsSettings{Name: "janus.local"}
	if err := p.CertificateAllowed(context.Background(), "janus.local"); err != nil {
		t.Errorf("front-door name denied: %v", err)
	}
	none := &PermissionByJanus{app: func() (*App, error) { return nil, errors.New("no janus app") }}
	if err := none.CertificateAllowed(context.Background(), "shop.local"); err == nil {
		t.Error("a config without the janus app minted")
	}
	// `permission janus` takes no arguments.
	if err := new(PermissionByJanus).UnmarshalCaddyfile(caddyfile.NewTestDispenser("janus")); err != nil {
		t.Errorf("bare form rejected: %v", err)
	}
	if err := new(PermissionByJanus).UnmarshalCaddyfile(caddyfile.NewTestDispenser("janus extra")); err == nil {
		t.Error("an argument was accepted")
	}
}
