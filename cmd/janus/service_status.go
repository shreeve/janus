package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	"github.com/spf13/cobra"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// exportRootCA fetches the local CA from the admin API at address (Caddy's
// address syntax: unix/<path> or host:port; empty is Caddy's default) and
// writes its root certificate, PEM, to path.
func exportRootCA(cmd *cobra.Command, address, path string) error {
	client, base := adminClient(address)
	defer client.CloseIdleConnections()
	resp, err := client.Get(base + "/pki/ca/local")
	if err != nil {
		return fmt.Errorf("admin API: %w (is the edge running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("admin API: %s from /pki/ca/local", resp.Status)
	}
	var ca struct {
		Root string `json:"root_certificate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ca); err != nil || ca.Root == "" {
		return errors.New("admin API: /pki/ca/local carried no root certificate")
	}
	if err := os.WriteFile(path, []byte(ca.Root), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
	return nil
}

func adminClient(address string) (*http.Client, string) {
	if sock, ok := strings.CutPrefix(address, "unix/"); ok {
		return controlClient(func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}), "http://caddy"
	}
	if address == "" {
		address = caddy.DefaultAdminListen
	}
	return controlClient(nil), "http://" + strings.TrimPrefix(address, "http://")
}

// edgeStatus is what 'janus status' knows, in one value.
type edgeStatus struct {
	Running     bool   `json:"running"`
	PID         int    `json:"pid,omitempty"`
	Uptime      string `json:"uptime,omitempty"`
	Supervisor  string `json:"supervisor,omitempty"` // item name, or "pidfile"
	Autostart   bool   `json:"autostart"`            // the item file exists
	Loaded      bool   `json:"loaded"`               // the manager holds the job
	Binary      string `json:"binary"`
	Version     string `json:"version"`
	Janus       string `json:"janus"`
	Caddy       string `json:"caddy"`
	BinaryNewer bool   `json:"binary_newer"`
	Config      string `json:"config"`
	Sites       string `json:"sites"`
	Env         string `json:"env"`
	State       string `json:"state"`
	Socket      string `json:"socket"` // control internal socket
	Admin       string `json:"admin"`  // Caddy admin socket
	Log         string `json:"log"`
	Control     string `json:"control,omitempty"`
	Apps        *int   `json:"apps,omitempty"` // only when control answered
	// The local CA that signs .local and .localhost names: its root
	// certificate on disk, whether this machine's trust store accepts it,
	// and the front door a phone trusts it from (when mdns answered).
	CA        string `json:"ca,omitempty"`
	CATrusted *bool  `json:"ca_trusted,omitempty"`
	TrustURL  string `json:"trust_url,omitempty"`
	// The exposure mode. Bind is what JANUS_BIND carries; Firewall is
	// verified / missing / unverified / none; Listeners are the front-door
	// addresses the mode admits, each with how its reach is enforced.
	Scope      string           `json:"scope"`
	Interface  string           `json:"interface,omitempty"`
	Bind       []string         `json:"bind"`
	Firewall   string           `json:"firewall"`
	Listeners  []listenerStatus `json:"listeners,omitempty"`
	ScopeError string           `json:"scope_error,omitempty"` // scope.json is unusable
}

type listenerStatus struct {
	Role  string `json:"role"`
	Addr  string `json:"addr"`
	Reach Reach  `json:"reach"`
}

func gatherStatus(p servicePaths) edgeStatus {
	st := edgeStatus{Config: p.config, Sites: p.sites, Env: p.env, State: p.state, Socket: p.sock, Admin: p.admin, Log: p.log, Version: versionLine(), Janus: janusVersion(), Caddy: caddyVersion()}
	st.Binary, _ = os.Executable()
	gatherExposure(&st, p)
	if item := itemFor(p); item != nil {
		st.Autostart = item.registered()
		st.Loaded, st.PID = item.loaded()
		if st.PID > 0 && !processAlive(st.PID) {
			st.PID = 0
		}
		if st.Loaded || st.Autostart {
			st.Supervisor = item.name()
		}
	}
	if st.PID == 0 {
		if fp := pidfilePID(p); fp > 0 {
			st.PID = fp
			st.Supervisor = "pidfile"
		}
	}
	if n, at := probeControl(p); at != "" {
		st.Control = at
		st.Apps = &n
		st.TrustURL = trustURL(p)
	}
	st.Running = st.PID > 0 || st.Control != ""
	gatherCA(&st)
	if st.PID > 0 {
		st.Uptime = elapsed(st.PID)
		st.BinaryNewer = binaryNewerThan(st.Binary, st.PID)
	}
	return st
}

// gatherExposure fills the mode fields: the stored scope, its bind on this
// OS, the firewall as far as this invocation can see it (root verifies;
// anyone else gets "unverified", never a claim), and the admitted
// addresses labeled by what enforces their reach.
func gatherExposure(st *edgeStatus, p servicePaths) {
	st.Bind = []string{}
	sc, err := readScope(p)
	if err != nil {
		st.ScopeError = err.Error()
		st.Firewall = "unknown (scope unusable)"
		return
	}
	st.Scope, st.Interface = string(sc.Scope), sc.Interface
	st.Bind, _ = sc.bind(runtime.GOOS)
	verdict := checkFirewallVerdict(sc)
	st.Firewall = verdict.String()
	plan, err := PlanListeners(sc.Scope, sc.lan())
	if err != nil {
		st.ScopeError = err.Error()
		return
	}
	for _, l := range plan {
		st.Listeners = append(st.Listeners, listenerStatus{Role: l.Role.String(), Addr: l.Addr.String(), Reach: classifyReach(l.Addr.Addr())})
	}
}

// localCARoot is the root certificate of Caddy's internal CA, where the
// pki app keeps it.
func localCARoot() string {
	return filepath.Join(caddy.AppDataDir(), "pki", "authorities", "local", "root.crt")
}

// verifyCA asks this machine's trust store whether it accepts the root:
// an empty verify against the system roots. Tests substitute it.
var verifyCA = func(root *x509.Certificate) error {
	_, err := root.Verify(x509.VerifyOptions{})
	return err
}

func gatherCA(st *edgeStatus) {
	path := localCARoot()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return
	}
	root, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	st.CA = path
	trusted := verifyCA(root) == nil
	st.CATrusted = &trusted
}

// trustURL is where a phone trusts the CA: the mdns front door, by its
// effective name, when the edge announces one.
func trustURL(p servicePaths) string {
	body, _, err := controlGet(p, "/1.0/mdns/status")
	if err != nil {
		return ""
	}
	var snap struct {
		Name          string `json:"name"`
		EffectiveName string `json:"effective_name"`
	}
	if json.Unmarshal(body, &snap) != nil {
		return ""
	}
	name := snap.EffectiveName
	if name == "" {
		name = snap.Name
	}
	if name == "" {
		return ""
	}
	return "http://" + name + "/trust"
}

func statusEdge(p servicePaths, cmd *cobra.Command, asJSON bool, note string) error {
	out := cmd.OutOrStdout()
	st := gatherStatus(p)
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			return err
		}
	} else {
		if note != "" {
			fmt.Fprintln(out, note)
		}
		printStatus(out, p, st)
	}
	if !st.Running {
		// Exit 3 (LSB "not running") for scripts; the output already
		// said it, so no error text.
		cmd.SilenceErrors = true
		return &exitError{code: 3, err: errors.New("janus is not running")}
	}
	return nil
}

func printStatus(out io.Writer, p servicePaths, st edgeStatus) {
	switch {
	case st.Running && st.PID > 0 && st.Uptime != "":
		fmt.Fprintf(out, "edge     running (pid %d, up %s)\n", st.PID, st.Uptime)
	case st.Running && st.PID > 0:
		fmt.Fprintf(out, "edge     running (pid %d)\n", st.PID)
	case st.Running:
		fmt.Fprintln(out, "edge     running (pid unknown: answered on control)")
	default:
		fmt.Fprintln(out, "edge     stopped")
	}
	switch {
	case st.Supervisor == "pidfile" && st.Autostart:
		fmt.Fprintf(out, "under    pidfile %s (autostart on, but the item is not running it: 'janus restart' hands it over)\n", p.pid)
	case st.Supervisor == "pidfile":
		fmt.Fprintf(out, "under    pidfile %s\n", p.pid)
	case st.Autostart && st.Loaded:
		fmt.Fprintf(out, "under    %s (autostart on)\n", st.Supervisor)
	case st.Autostart:
		fmt.Fprintf(out, "under    %s (autostart on, not loaded: 'janus start')\n", st.Supervisor)
	case st.Loaded:
		fmt.Fprintf(out, "under    %s (autostart off; the job stays until logout or 'janus stop')\n", st.Supervisor)
	default:
		fmt.Fprintln(out, "under    nothing (not under autostart)")
	}
	fmt.Fprintf(out, "binary   %s\nversion  %s\n", st.Binary, st.Version)
	if st.BinaryNewer {
		fmt.Fprintln(out, "         the binary is newer than the running edge: 'janus restart' to apply")
	}
	fmt.Fprintf(out, "config   %s\n", st.Config)
	fmt.Fprintf(out, "sites    %s\n", st.Sites)
	fmt.Fprintf(out, "log      %s\n", st.Log)
	switch {
	case st.Apps != nil:
		fmt.Fprintf(out, "control  %s (%d app%s registered)\n", st.Control, *st.Apps, plural(*st.Apps))
	case st.Running:
		fmt.Fprintf(out, "control  unreachable at %s and %s\n", p.sock, localControlURL)
	}
	switch {
	case st.CATrusted != nil && *st.CATrusted:
		fmt.Fprintln(out, "ca       trusted on this machine")
	case st.CATrusted != nil:
		fmt.Fprintln(out, "ca       not trusted on this machine: browsers warn on its https names ('janus trust')")
	}
	if st.TrustURL != "" {
		fmt.Fprintf(out, "         phones and other devices trust it at %s\n", st.TrustURL)
	}
	printExposure(out, st)
}

func printExposure(out io.Writer, st edgeStatus) {
	if st.ScopeError != "" {
		fmt.Fprintf(out, "scope    UNUSABLE: %s\n", st.ScopeError)
		return
	}
	scope := st.Scope
	if st.Interface != "" {
		scope += " (" + st.Interface + ")"
	}
	fmt.Fprintf(out, "scope    %s\n", scope)
	bind := strings.Join(st.Bind, " ")
	if runtime.GOOS == "darwin" && st.Scope != string(ScopeWAN) {
		bind += "  (wildcard socket; pf scopes it)"
	}
	fmt.Fprintf(out, "bind     %s\n", bind)
	fmt.Fprintf(out, "firewall %s\n", st.Firewall)
	label := "https    "
	for _, l := range st.Listeners {
		if l.Role != "https" {
			continue
		}
		fmt.Fprintf(out, "%s%-24s %s\n", label, l.Addr, l.Reach)
		label = "         "
	}
	if len(st.Listeners) > 0 {
		fmt.Fprintln(out, "http     the same addresses on port 80")
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// elapsed reports how long the process has been up, as ps prints it.
func elapsed(pid int) string {
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// binaryNewerThan reports whether the executable on disk was modified
// after the process started: an installed upgrade the edge is not running.
func binaryNewerThan(exe string, pid int) bool {
	st, err := os.Stat(exe)
	if err != nil {
		return false
	}
	up := parseElapsed(elapsed(pid))
	if up <= 0 {
		return false
	}
	started := time.Now().Add(-up)
	return st.ModTime().After(started.Add(2 * time.Second))
}

// parseElapsed reads ps's etime: [[dd-]hh:]mm:ss.
func parseElapsed(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		days, _ = strconv.Atoi(s[:i])
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var secs int
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second
}
