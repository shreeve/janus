package main

// The exposure verbs: mode, firewall, and the scope flags on autostart. They own the scope file; run, reload, and validate only read it
// (setBindEnv). Everything under the user's home is written by the user:
// the one privileged step, the pf rule on macOS, runs as `sudo janus
// firewall`, which reads the scope and touches only /etc and
// /Library. A verb that needs that step re-runs it under sudo when a
// terminal can prompt, and otherwise refuses before changing anything.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// exitNeedsRoot is the exit status for "this needs sudo, and there was no
// terminal to ask on".
const exitNeedsRoot = 4

// scopeFlags are the exposure options a verb takes.
type scopeFlags struct {
	scope string
	iface string
	v4    string
}

func addScopeFlags(cmd *cobra.Command, withScope bool) {
	if withScope {
		cmd.Flags().String("scope", "", "Exposure mode: localhost, lan, or wan")
	}
	cmd.Flags().String("interface", "", "For lan: the interface to use instead of the default route's ('auto' returns to that)")
	cmd.Flags().String("v4", "", "For lan: the interface's private IPv4 address to use, when it has several")
}

func readScopeFlags(cmd *cobra.Command) scopeFlags {
	var f scopeFlags
	if cmd.Flags().Lookup("scope") != nil {
		f.scope, _ = cmd.Flags().GetString("scope")
	}
	f.iface, _ = cmd.Flags().GetString("interface")
	f.v4, _ = cmd.Flags().GetString("v4")
	return f
}

// resolveScope turns the flags into a state. For lan the interface is the
// default route's, unless --interface names one (pinned: kept until unpinned)
// or the stored state already pinned one; --interface auto unpins.
func resolveScope(scope Scope, f scopeFlags, prev scopeState) (scopeState, error) {
	if scope != ScopeLAN {
		if f.iface != "" || f.v4 != "" {
			return scopeState{}, fmt.Errorf("--interface and --v4 belong to lan; %s binds no interface address", scope)
		}
		return scopeState{Scope: scope}, nil
	}
	iface, pinned := f.iface, true
	switch {
	case iface == "auto":
		iface, pinned = "", false
	case iface != "":
	case prev.Scope == ScopeLAN && prev.Pinned:
		iface = prev.Interface
	default:
		pinned = false
	}
	if iface == "" {
		var err error
		if iface, err = defaultInterface(); err != nil {
			return scopeState{}, err
		}
	}
	facts, err := interfaceFacts(iface)
	if err != nil {
		return scopeState{}, err
	}
	lan, on, err := chooseLAN(facts, f.v4)
	if err != nil {
		return scopeState{}, err
	}
	return scopeState{Scope: ScopeLAN, Interface: iface, Pinned: pinned, LANV4: lan.String(), OnLinkV4: on.String()}, nil
}

func stdinIsTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// canSudo: a terminal to prompt on, and the escalation not switched off
// (JANUS_NO_SUDO, for tests and for callers that want the refusal).
func canSudo() bool { return stdinIsTerminal() && os.Getenv("JANUS_NO_SUDO") == "" }

// firewallChange reports whether moving from prev to next touches the host
// firewall on this OS — the step that needs root. That is macOS only:
// entering a scoped mode, or leaving one whose anchor is on the host.
func firewallChange(prev, next scopeState, goos string, ops fwOps) bool {
	if NeedsFirewall(next.Scope, goos) {
		return true
	}
	return NeedsFirewall(prev.Scope, goos) && pfPresent(ops)
}

// sudoFirewall runs `janus firewall` under sudo for the invoking user's
// edge (the sudo run sees SUDO_UID: delegatedPaths). Without a terminal it
// refuses with the command to run; callers check canSudo before changing
// anything, so this refusal is for the case they did not.
func sudoFirewall(cmd *cobra.Command, why string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	line := "sudo " + exe + " firewall"
	if !canSudo() {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s needs root, and there is no terminal to ask on; run in a terminal: %s\n", why, line)
		cmd.SilenceErrors = true
		return &exitError{code: exitNeedsRoot, err: fmt.Errorf("%s needs root: %s", why, line)}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s needs root; running: %s\n", why, line)
	sudo := exec.Command("sudo", exe, "firewall")
	sudo.Stdin, sudo.Stdout, sudo.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := sudo.Run(); err != nil {
		return fmt.Errorf("%s: %v", line, err)
	}
	return nil
}

// ownPaths is the edge a scope-writing verb acts on: the caller's own.
// Root under sudo with no root edge is refused, so nothing under a user's
// home is ever written by root; the verb escalates by itself for the one
// step that needs it.
func ownPaths() (servicePaths, error) {
	p := currentPaths()
	if os.Geteuid() == 0 && !fileExists(p.config) {
		if uid := os.Getenv("SUDO_UID"); uid != "" && uid != "0" {
			return p, errors.New("this verb acts on the edge of the user running it; run it without sudo (it asks for sudo when the firewall step needs it)")
		}
	}
	return p, nil
}

// delegatedPaths is the edge a read-only or firewall verb reports on or
// applies for. Root acts on its own edge when it has one; under sudo with
// no root edge on the host it acts on the invoking user's. That is how
// `sudo janus firewall` and `sudo janus status` reach a user's edge on
// macOS, where the pf step needs root and the edge does not. Linux user
// edges never need a privileged verb (no mode needs a rule there), so no
// delegation happens there.
func delegatedPaths() (servicePaths, string) {
	p := currentPaths()
	if os.Geteuid() != 0 || fileExists(p.config) || runtime.GOOS != "darwin" {
		return p, ""
	}
	uidText := os.Getenv("SUDO_UID")
	uid, err := strconv.Atoi(uidText)
	if err != nil || uid == 0 {
		return p, ""
	}
	u, err := user.LookupId(uidText)
	if err != nil {
		return p, ""
	}
	gid, _ := strconv.Atoi(u.Gid)
	up := servicePathsFor(false, u.HomeDir, func(string) string { return "" })
	up.uid, up.gid = uid, gid
	return up, fmt.Sprintf("acting on %s's edge (root has no service Caddyfile of its own)", u.Username)
}

// setScope is the core of mode: store the state, apply or
// remove the firewall, and put the edge on the new bind. A firewall failure
// restores the previous state so scope.json never claims more than the host
// enforces; without a way to reach root, nothing is changed at all.
func setScope(cmd *cobra.Command, caddyReload *cobra.Command, p servicePaths, prev, next scopeState) error {
	out := cmd.OutOrStdout()
	root := os.Geteuid() == 0
	change := firewallChange(prev, next, runtime.GOOS, hostFwOps())
	why := "the firewall step for " + string(next.Scope)
	if change && !root && !canSudo() {
		return sudoFirewall(cmd, why)
	}
	if err := writeScope(p, next); err != nil {
		return err
	}
	bind, err := next.bind(runtime.GOOS)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "scope    %s%s\nbind     %s\n", next.Scope, ifaceSuffix(next), strings.Join(bind, " "))
	var verdict firewallVerdict
	switch {
	case change && root:
		verdict, err = applyFirewall(hostFwOps(), next)
	case change:
		err = sudoFirewall(cmd, why)
	default:
		verdict = checkFirewallVerdict(next)
	}
	if err != nil {
		if prev == next {
			return err
		}
		if rerr := writeScope(p, prev); rerr != nil {
			return fmt.Errorf("%v; and restoring the previous scope failed: %v", err, rerr)
		}
		return fmt.Errorf("%v; scope left at %s", err, prev.Scope)
	}
	if !(change && !root) {
		fmt.Fprintf(out, "firewall %s\n", verdict)
	}
	if next.Scope == ScopeWAN {
		fmt.Fprintln(out, "wildcard exposure active: the edge answers on every interface")
	}
	return applyToEdge(cmd, caddyReload, p, bind)
}

func ifaceSuffix(st scopeState) string {
	switch {
	case st.Interface == "":
		return ""
	case st.Pinned:
		return " (" + st.Interface + ", pinned)"
	}
	return " (" + st.Interface + ")"
}

// checkFirewallVerdict is the firewall's state for a scope, as far as this
// invocation can see it: root sees all of it; anyone else sees the files
// (missing is missing) and cannot see whether pf holds them (unverified).
func checkFirewallVerdict(st scopeState) firewallVerdict {
	if !NeedsFirewall(st.Scope, runtime.GOOS) {
		return firewallVerdict{}
	}
	if os.Geteuid() != 0 {
		if detail := pfInstalled(hostFwOps(), st); detail != "" {
			return firewallVerdict{needed: true, detail: detail}
		}
		return firewallVerdict{needed: true, unverified: true}
	}
	return checkFirewall(hostFwOps(), st)
}

// applyToEdge puts a running edge on the new bind with a reload, or starts
// an installed edge that is not running; a bare stopped edge is told what
// applies it.
func applyToEdge(cmd *cobra.Command, caddyReload *cobra.Command, p servicePaths, bind []string) error {
	out := cmd.OutOrStdout()
	if !fileExists(p.config) {
		fmt.Fprintf(out, "no Caddyfile at %s yet; 'janus autostart' seeds one on this scope\n", p.config)
		return nil
	}
	if runningPID(p) > 0 || controlReachable(p) {
		if err := loadEnvFile(p.env); err != nil {
			return err
		}
		_ = os.Setenv(bindEnvKey, strings.Join(bind, " "))
		_ = caddyReload.Flags().Set("config", p.config)
		_ = caddyReload.Flags().Set("adapter", "caddyfile")
		if err := caddyReload.RunE(caddyReload, nil); err != nil {
			return fmt.Errorf("reload: %w (the edge keeps its previous bind; its watch stops it if that is wider than the mode)", err)
		}
		fmt.Fprintln(out, "edge reloaded on the new bind")
		return nil
	}
	if item := itemFor(p); item != nil && item.registered() {
		if err := item.load(); err != nil {
			return err
		}
		fmt.Fprintf(out, "janus started under %s\n", item.name())
		return nil
	}
	fmt.Fprintln(out, "the edge is not running; 'janus start' runs it on this scope")
	return nil
}

// --- verbs -------------------------------------------------------------------

func modeCommand(caddyReload *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use: "mode [localhost|lan|wan]",
		Long: `
With no argument, prints the stored exposure mode, its bind, and the
firewall verdict for it, as 'janus status' does.

With one, sets the edge's exposure mode and applies it: the scope is
stored, the host firewall rule the mode needs is installed (or removed),
and a running edge is reloaded onto the new bind. Setting the mode it
already has resolves everything again, which is what to do after DHCP
moved the lan address or the default route moved to another interface.

  localhost  127.0.0.1 and ::1 only — this machine, absolutely.
  lan        localhost plus one interface's private IPv4 address — this
             machine and its local network. A private address is
             unreachable from the internet by addressing alone. The
             interface is the default route's; --interface <name> pins
             another (--interface auto returns to the default route), and
             when it has several private addresses --v4 names the one.
  wan        every interface — anyone the network lets through.

On macOS the socket is always the wildcard (an unprivileged process cannot
bind an exact low port) and pf enforces the mode; that step needs root, so
on a terminal the verb runs 'sudo janus firewall' for your edge. On Linux
the edge binds the mode's exact addresses and no firewall is involved.
Every mode is the default_bind the service Caddyfile reads through
{$JANUS_BIND}.
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return showMode(cmd)
			}
			scope, err := ParseScope(args[0])
			if err != nil {
				return err
			}
			f := readScopeFlags(cmd)
			p, err := ownPaths()
			if err != nil {
				return err
			}
			prev, err := readScope(p)
			if err != nil {
				return err
			}
			next, err := resolveScope(scope, f, prev)
			if err != nil {
				return err
			}
			if err := platformExposure(p, next); err != nil {
				return err
			}
			return setScope(cmd, caddyReload, p, prev, next)
		},
	}
	addScopeFlags(cmd, false)
	return cmd
}

// showMode prints the stored mode the way setScope reports one it set.
func showMode(cmd *cobra.Command) error {
	if readScopeFlags(cmd) != (scopeFlags{}) {
		return errors.New("options go with a mode to set: janus mode lan --interface <name>")
	}
	p, note := delegatedPaths()
	st, err := readScope(p)
	if err != nil {
		return err
	}
	bind, err := st.bind(runtime.GOOS)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if note != "" {
		fmt.Fprintln(out, note)
	}
	fmt.Fprintf(out, "scope    %s%s\nbind     %s\nfirewall %s\n", st.Scope, ifaceSuffix(st), strings.Join(bind, " "), checkFirewallVerdict(st))
	return nil
}

func firewallCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "firewall",
		Long: `
Applies the host firewall rule the stored exposure mode needs, without
resolving anything anew: the pf anchor on macOS, and pf enabled, now and
at boot. Run it again after an OS update rewrites /etc/pf.conf. Linux
binds exact addresses and needs no rule. Under sudo, with no root edge on
the host, it applies the rule for your edge. 'janus status' reports
whether the rule is in force (root sees the firewall; without root the
files are checked and pf itself is unverified).
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, note := delegatedPaths()
			st, err := readScope(p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if note != "" {
				fmt.Fprintln(out, note)
			}
			if err := platformExposure(p, st); err != nil {
				return err
			}
			if !NeedsFirewall(st.Scope, runtime.GOOS) && !(runtime.GOOS == "darwin" && pfPresent(hostFwOps())) {
				fmt.Fprintf(out, "scope    %s\nfirewall none (this mode needs no host rule here)\n", st.Scope)
				return nil
			}
			if os.Geteuid() != 0 {
				return sudoFirewall(cmd, "the firewall rule for "+string(st.Scope))
			}
			v, err := applyFirewall(hostFwOps(), st)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "scope    %s%s\nfirewall %s\n", st.Scope, ifaceSuffix(st), v)
			return nil
		},
	}
	return cmd
}

// platformExposure refuses what this OS cannot enforce: exposure modes on
// Windows (deferred).
func platformExposure(servicePaths, scopeState) error {
	switch runtime.GOOS {
	case "darwin", "linux":
		return nil
	}
	return fmt.Errorf("exposure modes are not supported on %s yet; use the portable high-port mode", runtime.GOOS)
}

// autostartScope settles the scope for autostart: the flags when given,
// else what is stored, else localhost. It resolves; the caller stores it.
func autostartScope(cmd *cobra.Command, p servicePaths) (prev, next scopeState, err error) {
	f := readScopeFlags(cmd)
	if prev, err = readScope(p); err != nil {
		return prev, prev, err
	}
	next = prev
	if f.scope != "" {
		scope, err := ParseScope(f.scope)
		if err != nil {
			return prev, prev, err
		}
		if next, err = resolveScope(scope, f, prev); err != nil {
			return prev, prev, err
		}
	} else if f.iface != "" || f.v4 != "" {
		return prev, prev, errors.New("--interface and --v4 go with --scope lan")
	}
	if err := platformExposure(p, next); err != nil {
		return prev, prev, err
	}
	return prev, next, nil
}

// refuseScopeFlags: start and reload take no exposure options; the mode
// belongs to the installed edge.
func refuseScopeFlags(cmd *cobra.Command) error {
	for _, f := range []string{"scope", "interface"} {
		if cmd.Flags().Changed(f) {
			return fmt.Errorf("--%s belongs to the edge's exposure mode: 'janus mode <scope>' changes it (and 'janus autostart --scope' installs with it)", f)
		}
	}
	return nil
}

func hiddenScopeFlags(cmd *cobra.Command) {
	cmd.Flags().String("scope", "", "")
	cmd.Flags().String("interface", "", "")
	_ = cmd.Flags().MarkHidden("scope")
	_ = cmd.Flags().MarkHidden("interface")
}
