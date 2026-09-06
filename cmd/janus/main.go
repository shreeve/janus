// Command janus is the Janus edge binary: stock Caddy compiled together
// with the Janus module and the Route 53 DNS provider (DNS-01 wildcard
// issuance) as one static executable named janus.
//
// Janus owns the command tree. Caddy's cmd package hard-codes "caddy" as
// the command to run throughout its help text and captures each command's
// text at registration, so the binary builds its own cobra root with its
// own description and examples, attaches every command Caddy registered
// with that text made accurate for a binary named janus, and provides the
// manpage and completion commands itself. The commands and their behavior
// are Caddy's; only what the help calls them changes.
package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	_ "github.com/caddy-dns/route53"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
	_ "github.com/shreeve/janus"
)

// version is stamped by release builds (-ldflags "-X main.version=$tag");
// source builds fall back to the VCS-stamped module version.
var version string

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "-v", "-V", "--version":
			fmt.Println(versionLine())
			return
		}
	}
	caddy.CustomBinaryName = "janus"
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(exitCode(err))
	}
}

func versionLine() string {
	return fmt.Sprintf("janus %s (caddy %s)", janusVersion(), caddyVersion())
}

const rootLong = `Janus is an edge server for disposable worker pools: TLS admission, dynamic
host routing, registry-driven upstreams, heartbeats, on-demand TLS asks,
edge-terminated WebSocket fan-out, LAN presence over mDNS, an edge
authentication wall, registered static files and directory browsing,
X-Sendfile offload, and bounded access observation, all driven by a JSON
control API on /1.0.

The janus binary is stock Caddy compiled together with the Janus module and
the Route 53 DNS provider as one executable. Every Caddy command is here
under the janus name; there is no separate caddy command on a Janus host.

To run Janus, use:

	- 'janus run' to run Janus in the foreground (recommended).
	- 'janus start' to start Janus in the background; only do this
	  if you will be keeping the terminal window open until you run
	  'janus stop' to close the server.

Configuration is an ordinary Caddyfile (see
https://caddyserver.com/docs/caddyfile) with a global janus block for the
capabilities and a janus directive in each site that admits traffic. If a
file named Caddyfile is in the current working directory, 'janus run' loads
it automatically; otherwise pass --config. Use 'janus adapt' to see how a
Caddyfile translates to Caddy's native JSON, and 'janus validate' to check
one without starting the server.

Janus-specific commands:

	- 'janus janus-auth-hash' mints a credential for the auth capability.
	- 'janus version' prints the Janus and Caddy versions.

Depending on the system, Janus may need permission to bind to low ports.
One way to do this on Linux is to use setcap:

	$ sudo setcap cap_net_bind_service=+ep $(which janus)

Remember to run that command again after replacing the binary.

Janus documentation, Caddyfile examples, and the control API contracts:
https://github.com/shreeve/janus

The remaining commands are Caddy's own; Caddy's command-line reference
documents them in full: https://caddyserver.com/docs/command-line
`

const rootExample = `  $ janus run
  $ janus run --config Caddyfile
  $ janus reload --config Caddyfile
  $ janus stop`

const helpFooter = `Janus documentation: https://github.com/shreeve/janus
Caddy's command-line reference (these commands, in full):
https://caddyserver.com/docs/command-line`

// newRootCommand builds the janus command tree from Caddy's registered
// commands.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "janus",
		Short:   "Janus edge server",
		Long:    rootLong,
		Example: rootExample,
		// Keep provisioning errors readable: no usage dump after them.
		SilenceUsage: true,
		Version:      versionLine(),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpTemplate(root.HelpTemplate() + "\n" + helpFooter + "\n")

	registered := caddycmd.Commands()
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		root.AddCommand(toCobra(registered[name]))
	}
	root.AddCommand(manpageCommand())
	// cobra supplies the completion command, generated for a root named
	// janus; add it now rather than lazily at Execute so the tree is
	// complete for manpage generation and for anyone walking it.
	root.InitDefaultCompletionCmd()
	return root
}

// toCobra mirrors Caddy's own conversion of a registered Command into a
// cobra command, with the help text made accurate for this binary.
func toCobra(c caddycmd.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   strings.TrimSpace(c.Name + " " + c.Usage),
		Short: janusify(c.Short),
		Long:  janusify(c.Long),
	}
	switch c.Name {
	case "upgrade", "add-package", "remove-package":
		// These operate on stock Caddy binaries downloaded from
		// caddyserver.com; a Janus build has nothing for them to manage,
		// and Caddy's package machinery dereferences module metadata
		// that exists only for dependency-built plugins.
		cmd.Short = "Not available on Janus (rebuild from source to change modules)"
		cmd.Long = "\n" + cmd.Short + ".\n"
		cmd.Hidden = true
		cmd.RunE = func(*cobra.Command, []string) error {
			return fmt.Errorf("janus: %q applies to stock Caddy binaries; rebuild Janus from source to change modules", c.Name)
		}
		return cmd
	}
	if c.CobraFunc != nil {
		c.CobraFunc(cmd)
		// Subcommands the hook attached (storage export/import) carry
		// Caddy's text too.
		for _, sub := range cmd.Commands() {
			sub.Short = janusify(sub.Short)
			sub.Long = janusify(sub.Long)
		}
	} else {
		cmd.RunE = caddycmd.WrapCommandFuncForCobra(c.Func)
		cmd.Flags().AddGoFlagSet(c.Flags)
	}
	return cmd
}

// manpageCommand generates section 8 manual pages for the janus tree.
func manpageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manpage --directory <path>",
		Short: "Generates the manual pages for Janus commands",
		Long: `
Generates the manual pages for Janus commands into the designated directory,
tagged into section 8 (System Administration).

The manual page files are generated into the directory specified by the
argument of --directory. If the directory does not exist, it will be created.
`,
	}
	cmd.Flags().StringP("directory", "o", "", "The output directory where the manpages are generated")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		dir := strings.TrimSpace(cmd.Flag("directory").Value.String())
		if dir == "" {
			cmd.SilenceErrors = true
			return &exitError{code: caddy.ExitCodeFailedQuit, err: errors.New("designated output directory is required")}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return doc.GenManTree(cmd.Root(), &doc.GenManHeader{Title: "Janus", Section: "8"}, dir)
	}
	return cmd
}

// janusify rewrites Caddy's help text for a binary named janus: the
// command to type is janus, and the process it runs is Janus. Caddy's own
// nouns stay — Caddyfile, Caddy's native JSON, Caddy modules, Caddy's
// storage, the caddyfile adapter, and caddyserver.com links — because
// those are accurate.
func janusify(s string) string {
	for _, r := range janusRewrites {
		s = r.re.ReplaceAllString(s, r.to)
	}
	return s
}

var janusRewrites = []struct {
	re *regexp.Regexp
	to string
}{
	// The command to type: 'caddy run', "caddy run", $ caddy run, > caddy run.
	{regexp.MustCompile(`(['"$>] ?)caddy `), "${1}janus "},
	{regexp.MustCompile(`\bwhich caddy\b`), "which janus"},
	// The process that runs.
	{regexp.MustCompile(`\bCaddy process\b`), "Janus process"},
	{regexp.MustCompile(`\bCaddy instances?\b`), "Janus instance"},
	{regexp.MustCompile(`\bruns Caddy\b`), "runs Janus"},
	{regexp.MustCompile(`\bstop Caddy\b`), "stop Janus"},
	{regexp.MustCompile(`\bconfigure Caddy\b`), "configure Janus"},
	{regexp.MustCompile(`\bgive Caddy\b`), "give Janus"},
	{regexp.MustCompile(`\bwhen Caddy\b`), "when Janus"},
	{regexp.MustCompile(`\bCaddy receives\b`), "Janus receives"},
	{regexp.MustCompile(`\bCaddy is started\b`), "Janus is started"},
}

// exitError carries an exit code from a command to main, like Caddy's.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

// exitCode recovers the exit status a command asked for. Caddy's own
// commands report theirs through cmd's unexported exitError, whose only
// exported surface is its ExitCode field, so it is read reflectively; the
// fallback is Caddy's generic failure status.
func exitCode(err error) int {
	for e := err; e != nil; e = errors.Unwrap(e) {
		var ours *exitError
		if errors.As(e, &ours) {
			return ours.code
		}
		v := reflect.ValueOf(e)
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		if v.Kind() == reflect.Struct {
			if f := v.FieldByName("ExitCode"); f.IsValid() && f.Kind() == reflect.Int {
				return int(f.Int())
			}
		}
	}
	return 1
}

// janusVersion reports the release tag without the leading v, preferring
// the ldflags stamp over the toolchain's VCS-derived module version.
func janusVersion() string {
	if version != "" {
		return strings.TrimPrefix(version, "v")
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return strings.TrimPrefix(v, "v")
		}
	}
	return "dev"
}

// caddyVersion reports the compiled Caddy dependency version without the
// leading v.
func caddyVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/caddyserver/caddy/v2" {
				return strings.TrimPrefix(dep.Version, "v")
			}
		}
	}
	return "unknown"
}
