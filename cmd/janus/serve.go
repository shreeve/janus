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

var serveReadyTimeout = 5 * time.Second

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
	st, err := readScope(p)
	if err != nil {
		return err
	}
	if st.Scope == ScopeWAN {
		return errors.New("the edge is in wan mode, and local names (<name>.localhost, <name>.local) are not served there; 'janus mode localhost' or 'janus mode lan' first")
	}
	client, root, err := openEdgeControl(p)
	if err != nil {
		cmd.SilenceErrors = true
		fmt.Fprintf(cmd.ErrOrStderr(), "janus is not running: no control plane answered at %s or %s\n", p.sock, localControlURL)
		return &exitError{code: 3, err: err}
	}
	defer client.Close()
	var caps struct {
		Browse       bool   `json:"browse"`
		HeartbeatTTL string `json:"heartbeat_ttl"`
	}
	if err := json.Unmarshal(root, &caps); err != nil || !caps.Browse {
		return fmt.Errorf("the edge's Caddyfile has browse off; 'browse' in its global janus block turns it on (the seed has it), then 'janus reload'")
	}
	heartbeat, err := heartbeatPeriod(caps.HeartbeatTTL)
	if err != nil {
		return err
	}
	// The site for this machine's names is janus's drop-in: put it in
	// place and apply it before the name is registered, so the first
	// request already has a certificate to answer with.
	if written, err := ensureLocalhostSite(p); err != nil {
		return err
	} else if written {
		fmt.Fprintf(out, "added %s (this machine's *.localhost names)\n", localhostSitePath(p))
		if err := reloadEdge(caddyReload, p); err != nil {
			path := localhostSitePath(p)
			contents, readErr := os.ReadFile(path)
			if readErr == nil && string(contents) == localhostSite(p) {
				readErr = os.Remove(path)
			} else if readErr == nil {
				readErr = fmt.Errorf("%s changed during reload; left it in place", path)
			}
			return fmt.Errorf("could not apply the localhost site: %w", errors.Join(err, readErr))
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

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	failures, stopHeartbeats := serveHeartbeats(ctx, client, reg.ID, heartbeat)
	defer stopHeartbeats()
	// A freshly applied site obtains its certificate in the background;
	// maintain the lease while waiting for usable HTTPS routing.
	problem := ""
	for deadline := time.Now().Add(serveReadyTimeout); ; {
		if problem = serveHandshake(hosts[0]); problem == "" || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-serveStop:
			return nil
		case err := <-failures:
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
	if problem != "" {
		return fmt.Errorf("the registered directory is not reachable: %s", problem)
	}
	fmt.Fprintf(out, "serving %s\n  https://%s/  (this machine)\n", dir, hosts[0])
	if st.Scope != ScopeLocalhost {
		fmt.Fprintf(out, "  https://%s/  (the network: %s mode)\n", hosts[1], st.Scope)
	}
	fmt.Fprintln(out, "if the browser warns about the certificate: janus trust   (Ctrl-C closes)")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-serveStop:
			return nil
		case err := <-failures:
			return err
		}
	}
}

func serveHeartbeats(parent context.Context, client *edgeControl, id string, interval time.Duration) (<-chan error, func()) {
	ctx, cancel := context.WithCancel(parent)
	failures, done := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, status, err := client.Do(http.MethodPost, "/1.0/apps/"+id+"/heartbeat", nil); err != nil || status/100 != 2 {
					failures <- fmt.Errorf("the edge stopped answering heartbeats (%d, %v); the registration is gone", status, err)
					return
				}
			}
		}
	}()
	return failures, func() { cancel(); <-done }
}

func heartbeatPeriod(ttl string) (time.Duration, error) {
	if ttl == "" {
		return serveHeartbeat, nil
	} // compatibility with older edges
	duration, err := time.ParseDuration(ttl)
	if err != nil || duration < 3*time.Millisecond {
		return 0, fmt.Errorf("the edge reported an invalid heartbeat_ttl %q", ttl)
	}
	return duration / 3, nil
}

// reloadEdge applies the service Caddyfile to the running edge, the way
// 'janus reload' does; a variable so tests need no admin socket.
var reloadEdge = func(_ *cobra.Command, p servicePaths) error {
	if err := loadEnvFile(p.env); err != nil {
		return err
	}
	if _, err := setBindEnv(p); err != nil {
		return err
	}
	return reloadConfig(p.config, "caddyfile", "", false)
}

// serveHandshake checks the directory route over loopback HTTPS and reports
// what would stop a browser, other than installing the local CA's trust.
// A variable so tests do not need a listener on 443.
var serveHandshake = func(host string) string {
	return probeServeRoute(host, "127.0.0.1:443")
}

func probeServeRoute(host, address string) string {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return err.Error()
	}
	req.Header.Set("Accept", "text/html")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("the edge did not serve HTTPS for %s (%v); check its sites import and on_demand_tls permission", host, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Sprintf("https://%s/ returned %s", host, response.Status)
	}
	return ""
}
