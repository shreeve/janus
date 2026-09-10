package main

// A directory, opened through the running edge: one browse-only
// registration with the control API (the browse contract's terminal
// files-only form), heartbeats while the command lives, gone on Ctrl-C.
// The edge serves it with the browse capability — listing, theme,
// renderers — under the exposure mode it already has, so what is open
// is exactly what 'janus mode' says, and nothing else starts.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// serveHeartbeat is the registration's heartbeat period: a third of the
// default TTL the edge reaps at. A variable for tests.
var serveHeartbeat = 5 * time.Second

func serveHeartbeatPeriod() time.Duration { return serveHeartbeat }
func setServeHeartbeat(d time.Duration)   { serveHeartbeat = d }

// serveStop ends a serve from a test instead of a signal.
var serveStop chan struct{}

func serveCommand(caddyReload *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use: "serve [directory]",
		Long: `
Opens a directory (default: the current one) through the running edge with
the browse capability: a listing, the theme, and the renderers the edge is
configured with, at https://<name>.localhost/ on this machine and, in lan
mode, https://<name>.local/ for the network (announced over mDNS). The
name is the directory's, lowercased; --name chooses another.

Nothing new listens: the directory is registered with the edge over its
control API and served on the ports the exposure mode already scopes. The
registration lives while this command runs and is removed on Ctrl-C.

The edge's Caddyfile must have browse on (the seed does). The site for
<name>.localhost is janus's own drop-in, sites/localhost.caddy, written
here when it is missing and applied with a reload, whoever renders the
Caddyfile itself. If the browser warns about the certificate, 'janus
trust' installs the edge's CA on this machine.
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", abs)
			}
			name, _ := cmd.Flags().GetString("name")
			if name == "" {
				name = serveName(filepath.Base(abs))
			}
			if !validServeName(name) {
				return fmt.Errorf("--name %q: a name is lowercase letters, digits, and hyphens (one DNS label)", name)
			}
			p, note := delegatedPaths()
			if note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			// Local names are not served in wan mode; there is nothing to open.
			if st, err := readScope(p); err == nil && st.Scope == ScopeWAN {
				return errors.New("the edge is in wan mode, and local names (<name>.localhost, <name>.local) are not served there; 'janus mode localhost' or 'janus mode lan' first")
			}
			return serveDir(cmd, caddyReload, p, abs, name)
		},
	}
	cmd.Flags().String("name", "", "The name to serve as (<name>.localhost and <name>.local); default: the directory's")
	return cmd
}

var serveLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validServeName(name string) bool { return serveLabel.MatchString(name) }

// serveName makes one DNS label of a directory name: lowercase, runs of
// anything else become one hyphen, trimmed.
func serveName(base string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		default:
			if b.Len() > 0 && !hyphen {
				b.WriteByte('-')
				hyphen = true
			}
		}
	}
	name := strings.TrimRight(b.String(), "-")
	if name == "" {
		name = "files"
	}
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	return name
}

func serveDir(cmd *cobra.Command, caddyReload *cobra.Command, p servicePaths, dir, name string) error {
	out := cmd.OutOrStdout()
	client, root, err := openEdgeControl(p)
	if err != nil {
		cmd.SilenceErrors = true
		fmt.Fprintf(cmd.ErrOrStderr(), "janus is not running: no control plane answered at %s or %s\n", p.sock, localControlURL)
		return &exitError{code: 3, err: err}
	}
	defer client.Close()
	var caps struct {
		Browse bool `json:"browse"`
	}
	if err := json.Unmarshal(root, &caps); err != nil || !caps.Browse {
		return fmt.Errorf("the edge's Caddyfile has browse off; 'browse' in its global janus block turns it on (the seed has it), then 'janus reload'")
	}
	// The site for this machine's names is janus's drop-in: put it in
	// place and apply it before the name is registered, so the first
	// request already has a certificate to answer with.
	if written, err := ensureLocalhostSite(p); err != nil {
		return err
	} else if written {
		fmt.Fprintf(out, "added %s (this machine's *.localhost names)\n", localhostSitePath(p))
		if err := reloadEdge(caddyReload, p); err != nil {
			fmt.Fprintf(out, "note: the edge did not reload it (%v); 'janus reload' applies it\n", err)
		}
	}
	hosts := []string{name + ".localhost", name + ".local"}
	body, _ := json.Marshal(map[string]any{
		"name":      name,
		"hosts":     hosts,
		"upstreams": []string{},
		"files":     map[string]any{"roots": []map[string]any{{"path": dir, "cache": "revalidate", "browse": true}}},
		"lease":     "heartbeat",
	})
	created, status, err := client.Do(http.MethodPost, "/1.0/apps", body)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return fmt.Errorf("the name %s is taken on this edge (%s); choose another with --name", name, strings.TrimSpace(string(created)))
	}
	if status != http.StatusCreated {
		return fmt.Errorf("the edge refused the registration (%d): %s", status, strings.TrimSpace(string(created)))
	}
	var reg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &reg); err != nil || reg.ID == "" {
		return fmt.Errorf("the edge's answer has no id: %s", created)
	}
	defer func() {
		_, _, _ = client.Do(http.MethodDelete, "/1.0/apps/"+reg.ID, nil)
		fmt.Fprintf(out, "closed %s\n", dir)
	}()

	st, _ := readScope(p)
	fmt.Fprintf(out, "serving %s\n  https://%s/  (this machine)\n", dir, hosts[0])
	if st.Scope != ScopeLocalhost {
		fmt.Fprintf(out, "  https://%s/  (the network: %s mode)\n", hosts[1], st.Scope)
	}
	// A freshly applied site obtains its certificate in the background;
	// give it a few seconds before calling the handshake a problem.
	problem := ""
	for deadline := time.Now().Add(5 * time.Second); ; {
		if problem = serveHandshake(hosts[0]); problem == "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if problem != "" {
		fmt.Fprintf(out, "note: %s\n", problem)
	}
	fmt.Fprintln(out, "if the browser warns about the certificate: janus trust   (Ctrl-C closes)")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	tick := time.NewTicker(serveHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-serveStop:
			return nil
		case <-tick.C:
			if _, status, err := client.Do(http.MethodPost, "/1.0/apps/"+reg.ID+"/heartbeat", nil); err != nil || status/100 != 2 {
				return fmt.Errorf("the edge stopped answering heartbeats (%d, %v); the registration is gone", status, err)
			}
		}
	}
}

// reloadEdge applies the service Caddyfile to the running edge, the way
// 'janus reload' does; a variable so tests need no admin socket.
var reloadEdge = func(caddyReload *cobra.Command, p servicePaths) error {
	_ = caddyReload.Flags().Set("config", p.config)
	_ = caddyReload.Flags().Set("adapter", "caddyfile")
	return caddyReload.RunE(caddyReload, nil)
}

// serveHandshake asks the edge for the served name over TLS on the
// loopback and reports what would stop a browser, other than trust.
// A variable so tests do not need a listener on 443.
var serveHandshake = func(host string) string {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", "127.0.0.1:443", &tls.Config{ServerName: host, InsecureSkipVerify: true})
	if err != nil {
		return fmt.Sprintf("the edge did not complete a TLS handshake for %s (%v); its Caddyfile must import the sites directory (import <sites>/*.caddy) and its on_demand_tls permission must be janus (or an ask that admits registered names)", host, err)
	}
	conn.Close()
	return ""
}
