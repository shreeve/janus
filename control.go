package janus

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Default listen targets when a control line omits the address.
const (
	DefaultControlInternal = "run/janus.sock"
	DefaultControlLocal    = "http://127.0.0.1:7600/"
	DefaultControlPublic   = "https://0.0.0.0:7601/"
)

// Token kinds after parsing token:….
const (
	tokenEnv     = "env"
	tokenFile    = "file"
	tokenLiteral = "literal"
)

// Control is one self-contained control-plane listener.
type Control struct {
	// Mode is internal, local, or public.
	Mode string `json:"mode,omitempty"`

	// Listen is a unix path (internal) or http(s) URL (local/public).
	Listen string `json:"listen,omitempty"`

	// Token is the raw token:… suffix value (no "token:" prefix), if any.
	Token string `json:"token,omitempty"`

	// TokenKind is env, file, or literal.
	TokenKind string `json:"token_kind,omitempty"`

	// Everything below is derived at Provision, never configured.

	// basePath is the URL path prefix for the control API (local/public).
	basePath string

	// network and addr feed net.Listen.
	network string
	addr    string

	// useTLS is set for https:// listeners.
	useTLS bool

	// secret is the resolved Bearer token.
	secret string
}

// parseTokenArg parses a token:… argument.
//
//	token:JANUS_TOKEN     → env (unquoted, no "/")
//	token:./secrets/x     → file (contains "/")
//	"token:dev-only"      → literal (entire arg was quoted; not for public)
func parseTokenArg(val string, quoted bool) (kind, secretRef string, err error) {
	if !strings.HasPrefix(val, "token:") {
		return "", "", fmt.Errorf("token argument must start with %q", "token:")
	}
	ref := strings.TrimPrefix(val, "token:")
	if ref == "" {
		return "", "", fmt.Errorf("token: value is empty")
	}
	if quoted {
		return tokenLiteral, ref, nil
	}
	if strings.Contains(ref, "/") {
		return tokenFile, ref, nil
	}
	return tokenEnv, ref, nil
}

func resolveToken(kind, ref string) (string, error) {
	switch kind {
	case tokenEnv:
		v := os.Getenv(ref)
		if v == "" {
			return "", fmt.Errorf("environment variable %q is unset or empty", ref)
		}
		return v, nil
	case tokenFile:
		path := ref
		if strings.HasPrefix(path, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, path[2:])
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("token file %q is empty", path)
		}
		return v, nil
	case tokenLiteral:
		if ref == "" {
			return "", fmt.Errorf("literal token is empty")
		}
		return ref, nil
	default:
		return "", fmt.Errorf("unknown token kind %q", kind)
	}
}

func (c *Control) normalize() error {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	switch mode {
	case "internal":
		c.Mode = "internal"
		if c.Listen == "" {
			c.Listen = DefaultControlInternal
		}
		if strings.Contains(c.Listen, "://") {
			return fmt.Errorf("control internal wants a socket path, got URL %q", c.Listen)
		}
		c.network = "unix"
		c.addr = c.Listen
		c.basePath = ""
		c.useTLS = false
	case "local":
		c.Mode = "local"
		if c.Listen == "" {
			c.Listen = DefaultControlLocal
		}
		if err := c.parseHTTPListen(false); err != nil {
			return fmt.Errorf("control local: %w", err)
		}
	case "public":
		c.Mode = "public"
		if c.Listen == "" {
			c.Listen = DefaultControlPublic
		}
		if err := c.parseHTTPListen(true); err != nil {
			return fmt.Errorf("control public: %w", err)
		}
		if c.TokenKind == "" {
			return fmt.Errorf("control public requires token:… on the same line")
		}
		if c.TokenKind == tokenLiteral {
			return fmt.Errorf("control public cannot use a literal token (use token:ENV or token:./path)")
		}
	default:
		return fmt.Errorf("unknown control mode %q (want internal, local, or public)", c.Mode)
	}

	if c.TokenKind != "" {
		secret, err := resolveToken(c.TokenKind, c.Token)
		if err != nil {
			return fmt.Errorf("control %s token: %w", c.Mode, err)
		}
		c.secret = secret
	}
	return nil
}

func (c *Control) parseHTTPListen(requireHTTPS bool) error {
	u, err := url.Parse(c.Listen)
	if err != nil {
		return fmt.Errorf("invalid listen URL %q: %w", c.Listen, err)
	}
	switch u.Scheme {
	case "http":
		if requireHTTPS {
			return fmt.Errorf("public control requires https://, got %q", c.Listen)
		}
		c.useTLS = false
	case "https":
		c.useTLS = true
	default:
		return fmt.Errorf("listen URL must be http:// or https://, got %q", c.Listen)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if c.useTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	if host == "" {
		return fmt.Errorf("listen URL missing host: %q", c.Listen)
	}
	if !requireHTTPS {
		if host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return fmt.Errorf("local control must bind loopback (127.0.0.1, ::1, or localhost), got %q", host)
		}
	}
	c.network = "tcp"
	c.addr = net.JoinHostPort(host, port)
	c.basePath = strings.TrimSuffix(u.Path, "/")
	if c.basePath == "/" {
		c.basePath = ""
	}
	return nil
}
