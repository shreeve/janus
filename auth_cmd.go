package janus

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"golang.org/x/term"
)

// The credential minter: `caddy janus-auth-hash` (namespaced — a
// top-level `auth` command would risk colliding with upstream or other
// modules' command names). The password never rides argv (shell history
// is not a credential store): on a terminal it is prompted without echo
// and confirmed; with stdin redirected, exactly one line is read — so
// `printf 'pw\n' | caddy janus-auth-hash` works in scripts and test
// fixtures. Output is the one g1:… line the operator pastes after
// `user <name>`; the command runs the same constants the verifier runs.

func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "janus-auth-hash",
		Usage: "",
		Short: "Mints a Janus auth credential (g1 blob) from a password",
		Long: `
Mints a credential for the janus auth capability: argon2id under the
fixed g1 constants (m=64MiB, t=2, p=1; 16-byte salt, 32-byte key),
printed as one g1:<base64> line for a Caddyfile user line:

	user alice g1:…

On a terminal the password is prompted without echo and confirmed.
With stdin redirected, exactly one line is read as the password:

	printf 'secret\n' | caddy janus-auth-hash
`,
		Func: cmdJanusAuthHash,
	})
}

func cmdJanusAuthHash(fl caddycmd.Flags) (int, error) {
	password, err := readAuthPassword()
	if err != nil {
		return 1, err
	}
	if password == "" {
		return 1, errors.New("password must not be empty")
	}
	blob, err := g1Mint(password)
	if err != nil {
		return 1, err
	}
	fmt.Println(blob)
	return 0, nil
}

func readAuthPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Piped input: exactly one line, trailing newline stripped.
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password from stdin: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "Password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	fmt.Fprint(os.Stderr, "Confirm:  ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("reading confirmation: %w", err)
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}
