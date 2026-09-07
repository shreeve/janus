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

const rootLong = `Janus is the host's edge: one process that terminates TLS, routes each
host to the app registered for it, and carries the control API on /1.0.
The binary is Caddy built with the Janus module, so every Caddy command is
here under the janus name. The commands below are the ones an operator
runs; the rest still work by name (https://caddyserver.com/docs/command-line).
`

const helpFooter = `Files: ~/.config/janus/Caddyfile and ~/.local/state/janus for a user;
/etc/janus, /var/lib/janus, and /var/log/janus for root. The verbs default
to that Caddyfile. Docs and examples: https://github.com/shreeve/janus`

// shortOf is the one-line summary of every listed command, in one place
// so the help reads as one voice; Janus's own verbs define no Short of
// their own, and Caddy's are replaced.
var shortOf = map[string]string{
	"autostart":    "Install the edge as a service: running now, at every login (root: boot), and after a crash",
	"start":        "Start the installed edge",
	"stop":         "Stop it; it stays stopped until the next login or 'janus start'",
	"restart":      "Stop and start again (how an installed upgrade takes effect)",
	"reload":       "Apply an edited Caddyfile without dropping connections",
	"status":       "Running or not, under what, apps registered, and the exposure mode",
	"apps":         "What is registered with the edge: hosts, workers, leases",
	"logs":         "The edge's log, live (Ctrl-C stops); the last lines when piped",
	"run":          "Run in the foreground with a Caddyfile (./Caddyfile or --config)",
	"mode":         "Show or set where the edge listens: localhost (default), lan, or wan",
	"firewall":     "Re-apply the host firewall rule the mode needs (root; macOS)",
	"validate":     "Check the Caddyfile without touching the running edge",
	"trust":        "Trust the edge's local CA on this machine",
	"untrust":      "Remove that trust",
	"passhash":     "Mint a credential for the auth wall",
	"serve":        "Open a directory through the edge, with browse: https://<name>.localhost/",
	"file-server":  "Serve a directory on :8080 with Caddy's plain listing",
	"list-modules": "List the Caddy modules in this build",
	"version":      "Janus and Caddy versions",
	"completion":   "Shell completion script",
	"help":         "Help for a command",
}

// newRootCommand builds the janus command tree from Caddy's registered
// commands.
func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "janus",
		Short: "Janus edge server",
		Long:  rootLong,
		// Keep provisioning errors readable: no usage dump after them.
		SilenceUsage: true,
		Version:      versionLine(),
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.SetHelpTemplate(root.HelpTemplate() + "\n" + helpFooter + "\n")
	for _, g := range commandGroups {
		root.AddGroup(&cobra.Group{ID: g.id, Title: g.title})
	}

	registered := caddycmd.Commands()
	names := make([]string, 0, len(registered))
	for name := range registered {
		names = append(names, name)
	}
	sort.Strings(names)
	byName := map[string]*cobra.Command{}
	for _, name := range names {
		cmd := toCobra(registered[name])
		byName[name] = cmd
		root.AddCommand(cmd)
	}
	// The service verbs: Caddy's start/stop/reload/run/validate learn the
	// service's Caddyfile and supervisor, and restart/autostart/status join
	// them.
	for _, cmd := range serviceCommands(byName["run"], byName["start"], byName["stop"], byName["reload"], byName["validate"], byName["adapt"], byName["trust"], byName["untrust"]) {
		root.AddCommand(cmd)
	}
	root.AddCommand(manpageCommand())
	// cobra supplies the completion command, generated for a root named
	// janus; add it now rather than lazily at Execute so the tree is
	// complete for manpage generation and for anyone walking it.
	root.InitDefaultCompletionCmd()
	root.SetHelpCommandGroupID("tools")
	// The help lists each group in the order an operator meets its verbs,
	// not alphabetically: re-add in that order, the rest after.
	cobra.EnableCommandSorting = false
	all := root.Commands()
	root.ResetCommands()
	seen := map[string]bool{}
	for _, name := range helpOrder {
		for _, cmd := range all {
			if cmd.Name() == name && !seen[name] {
				seen[name] = true
				root.AddCommand(cmd)
			}
		}
	}
	for _, cmd := range all {
		if !seen[cmd.Name()] {
			root.AddCommand(cmd)
		}
	}
	root.InitDefaultHelpCmd()
	for _, cmd := range root.Commands() {
		if g, ok := groupOf[cmd.Name()]; ok {
			cmd.GroupID = g
		}
		if short, ok := shortOf[cmd.Name()]; ok {
			cmd.Short = short
		}
	}
	// The root's own -h and -v are not worth a Flags section of their own.
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	_ = root.Flags().MarkHidden("help")
	_ = root.Flags().MarkHidden("version")
	return root
}

// helpOrder is the visible commands as the help lists them.
var helpOrder = []string{
	"autostart", "start", "stop", "restart", "reload", "status", "apps", "logs", "serve", "run",
	"mode", "firewall",
	"validate", "trust", "untrust", "passhash",
	"file-server", "list-modules", "version", "completion", "help",
}

// The help is sectioned by what an operator is doing; a command outside
// these groups is Caddy's own and still works by name.
var commandGroups = []struct{ id, title string }{
	{"edge", "The edge:"},
	{"exposure", "Exposure:"},
	{"config", "Config and credentials:"},
	{"tools", "Tools:"},
}

var groupOf = map[string]string{
	"autostart": "edge", "start": "edge", "stop": "edge", "restart": "edge", "reload": "edge", "status": "edge", "apps": "edge", "logs": "edge", "serve": "edge", "run": "edge",
	"mode": "exposure", "firewall": "exposure",
	"validate": "config", "trust": "config", "untrust": "config", "passhash": "config",
	"file-server": "tools", "list-modules": "tools", "version": "tools", "completion": "tools",
}

// hiddenCommands still work by name and stay out of the help: Caddyfile
// developer tools, generators and build introspection, an experimental
// storage shell, the ad hoc servers that would contend with the edge for
// its ports, and Caddy's basic_auth minter, which mints a credential the
// Janus auth wall rejects (that is 'janus passhash').
var hiddenCommands = map[string]bool{
	"adapt": true, "fmt": true, "environ": true, "build-info": true, "manpage": true,
	"storage": true, "respond": true, "reverse-proxy": true, "hash-password": true,
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
	cmd.Hidden = hiddenCommands[c.Name]
	if c.Name == "file-server" {
		// Caddy's default is :80, which on a Janus host is the edge's
		// port; both would bind it (SO_REUSEPORT) and split the requests.
		if f := cmd.Flags().Lookup("listen"); f != nil {
			_ = f.Value.Set(":8080")
			f.DefValue = ":8080"
		}
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
	cmd.Hidden = hiddenCommands["manpage"]
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
