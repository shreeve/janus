package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/shreeve/janus"
	"github.com/spf13/cobra"
	"os"
	"strings"
)

func serviceCommands(caddyRun, caddyStart, caddyStop, caddyReload, caddyValidate, caddyAdapt, caddyTrust, caddyUntrust *cobra.Command) []*cobra.Command {
	p := currentPaths()

	wrap := func(cmd *cobra.Command, fn func(orig runFunc, cmd *cobra.Command, args []string) error) {
		orig := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error { return fn(orig, cmd, args) }
	}
	const serviceDefault = "\nThe service Caddyfile is the default --config when it exists.\n"

	// Caddy's own verbs learn the service's Caddyfile as their default.
	caddyRun.Long = janusify(caddyRun.Long) + `
With no Caddyfile in the current directory, the service Caddyfile is the
default when it exists, and the service env file is loaded beside it.
`
	wrap(caddyRun, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("config") {
			if _, err := os.Stat("Caddyfile"); err != nil && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
		}
		st, err := prepareConfigEnvironment(cmd, p)
		if err != nil {
			return err
		}
		if usesServiceConfig(cmd, p) {
			// The service edge: the mode must be enforceable here, the
			// host rule it needs must be in place before a wildcard
			// socket opens, nothing else may already answer on its
			// ports, and what it binds is watched against the mode for
			// as long as it runs.
			if err := serviceEdgeReady(st); err != nil {
				return err
			}
			janus.SetExposure(string(st.Scope), st.Interface, st.lan())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go exposureWatch(ctx, p)
		}
		return orig(cmd, args)
	})
	// adapt reads the Caddyfile too: the bind must be in its environment,
	// or {$JANUS_BIND} adapts to Caddy's own default, the wildcard.
	wrap(caddyAdapt, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if _, err := prepareConfigEnvironment(cmd, p); err != nil {
			return err
		}
		return orig(cmd, args)
	})
	hiddenScopeFlags(caddyReload)
	caddyReload.Flags().StringSlice("envfile", nil, "Environment file(s) to load")
	for _, c := range []*cobra.Command{caddyReload, caddyValidate} {
		c.Use = strings.Replace(c.Use, "--config <path>", "[--config <path>]", 1)
		c.Long = janusify(c.Long) + serviceDefault
		wrap(c, func(orig runFunc, cmd *cobra.Command, args []string) error {
			if err := refuseScopeFlags(cmd); err != nil {
				return err
			}
			// Only the service edge is known to be running or not; a
			// --config or --address names some other edge.
			if cmd.Name() == "reload" && targetsService(cmd) && runningPID(p) == 0 && !controlReachable(p) {
				return errors.New("janus is not running; 'janus start' runs the current Caddyfile")
			}
			if !cmd.Flags().Changed("config") && fileExists(p.config) {
				setConfig(cmd, p.config)
			}
			if _, err := prepareConfigEnvironment(cmd, p); err != nil {
				return err
			}
			if cmd.Name() == "reload" {
				config, _ := cmd.Flags().GetString("config")
				adapter, _ := cmd.Flags().GetString("adapter")
				address, _ := cmd.Flags().GetString("address")
				force, _ := cmd.Flags().GetBool("force")
				return reloadConfig(config, adapter, address, force)
			}
			return orig(cmd, args)
		})
	}
	caddyStop.Long = janusify(caddyStop.Long) + serviceDefault + `
Under an autostart item this is a clean exit, so the edge stays stopped
until the next login (root: boot); 'janus start' brings it back. When
nothing is running this is a quiet no-op.
`
	caddyStop.Flags().StringSlice("envfile", nil, "Environment file(s) to load")
	wrap(caddyStop, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("address") {
			return orig(cmd, args)
		}
		if targetsService(cmd) && runningPID(p) == 0 && !controlReachable(p) {
			fmt.Fprintln(cmd.OutOrStdout(), "janus was not running")
			return nil
		}
		if !cmd.Flags().Changed("config") && fileExists(p.config) {
			setConfig(cmd, p.config)
		}
		if _, err := prepareConfigEnvironment(cmd, p); err != nil {
			return err
		}
		return orig(cmd, args)
	})

	// trust and untrust talk to the admin API, which the service edge keeps
	// on its own socket rather than Caddy's default port; when that socket
	// is there, it is the address.
	for _, c := range []*cobra.Command{caddyTrust, caddyUntrust} {
		c.Long = janusify(c.Long) + `
The running service edge's admin socket is the default --address.
`
		c.PreRunE = func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("address") && socketExists(p.admin) {
				return cmd.Flags().Set("address", "unix/"+p.admin)
			}
			return nil
		}
	}
	caddyTrust.Long += `
--export writes the CA's root certificate to a file (PEM) instead of
trusting it here: for another machine's trust store. A phone takes it
from the edge itself, at http://<mdns name>/trust.
`
	caddyTrust.Flags().String("export", "", "write the root certificate (PEM) to this file instead of trusting it")
	wrap(caddyTrust, func(orig runFunc, cmd *cobra.Command, args []string) error {
		path, _ := cmd.Flags().GetString("export")
		if path == "" {
			return orig(cmd, args)
		}
		address, _ := cmd.Flags().GetString("address")
		return exportRootCA(cmd, address, path)
	})

	// start: under the item when there is one, else Caddy's background
	// start with the service's Caddyfile and pidfile as defaults.
	caddyStart.Long = `
Starts the edge in the background and returns.

With an autostart item installed ('janus autostart'), the item is what
runs the edge: this loads it, or starts it again after a 'janus stop'.
Start options belong to the item's Caddyfile then, and are refused here.

Without an item, this is Caddy's background start: the process detaches
and records its pid so 'janus status' and 'janus restart' can find it.
The service Caddyfile is the default --config when it exists, and the
service env file is loaded beside it.
`
	hiddenScopeFlags(caddyStart)
	wrap(caddyStart, func(orig runFunc, cmd *cobra.Command, args []string) error {
		if err := refuseScopeFlags(cmd); err != nil {
			return err
		}
		return startEdge(orig, cmd, args, p)
	})

	restart := &cobra.Command{
		Use: "restart",
		Long: `
Stops the running edge gracefully, waits for it to exit (up to 10 s), and
starts it again, under its autostart item when there is one. Connections
drop for the moment in between; a config change alone is better served by
'janus reload', which keeps them.

This is how an installed upgrade takes effect: install the new binary,
then 'janus restart'.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return restartEdge(caddyStop, caddyStart, p, cmd)
		},
	}

	autostart := &cobra.Command{
		Use: "autostart [off [stop]]",
		Long: `
Installs the edge as a service under the platform's manager — a launchd
item on macOS, a systemd unit on Linux — and starts it now. From then on it
comes back after a crash and starts at every login; as root, at every boot
with no login needed. A clean 'janus stop' stays stopped until then.

Run as yourself on a machine you log in to. Run as root ('sudo janus
autostart', with janus installed where root finds it: /usr/local/bin) for
a server: ports 80 and 443 with no setcap, up before anyone logs in. One
or the other on a host, never both.

The service Caddyfile is seeded with a runnable starting point when absent
— the control plane, the admin socket, a rolling process log, and an
import of the drop-in sites directory beside it — and validated before
the item is installed, so a bad config never becomes a restart loop. An
env file beside it (KEY=value lines) is loaded into the edge at start.

'janus autostart off' removes the item and leaves a running edge alone;
'janus autostart off stop' takes both down. Run again after changing the
binary and the item is rewritten; 'janus restart' applies it.

--scope sets the exposure mode the edge is installed with (localhost, the
default when none is stored; lan takes --interface to pin one).
Without --scope a stored mode is kept. 'janus mode' changes it later.
`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return autostartEdge(caddyStop, p, args, cmd)
		},
	}
	addScopeFlags(autostart, true)

	status := &cobra.Command{
		Use: "status",
		Long: `
Shows the edge: running or stopped (and under what: the autostart item, a
pidfile, or nothing), this binary and its version, whether the binary is
newer than the edge that is running, the service Caddyfile, sites
directory, and log, and the control endpoint with how many apps are
registered on it.

Then the exposure mode: scope, the bind the Caddyfile gets through
JANUS_BIND, whether the host firewall rule the mode needs is in force
(root sees the firewall; run under sudo to verify it), and each front-door
address with how its reach is enforced — by the host (loopback, a private
address, a verified firewall) or only by the network.

Exits 3 when the edge is not running. --json prints the same as one
object, plus the service's paths for tools that write site files or
register with the edge: running, pid, uptime, supervisor, autostart,
loaded, binary, version, janus, caddy, binary_newer, config, sites, env,
state, socket (the control socket), admin (Caddy's admin socket), log,
scope, interface, bind, firewall, listeners, and control with apps
(present only when control answered).
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			sp, note := delegatedPaths()
			return statusEdge(sp, cmd, asJSON, note)
		},
	}
	status.Flags().Bool("json", false, "Print the status as JSON")

	return []*cobra.Command{restart, autostart, status, appsCommand(), logsCommand(), serveCommand(caddyReload), modeCommand(caddyReload), firewallCommand()}
}

type runFunc = func(cmd *cobra.Command, args []string) error
