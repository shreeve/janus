package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

func sameConfigPath(a, b string) bool {
	left, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return false
	}
	if left == right {
		return true
	}
	li, le := os.Stat(left)
	ri, re := os.Stat(right)
	return le == nil && re == nil && os.SameFile(li, ri)
}

func usesServiceConfig(cmd *cobra.Command, p servicePaths) bool {
	cfg, _ := cmd.Flags().GetString("config")
	if cfg == "" {
		cfg = "Caddyfile"
	}
	return sameConfigPath(cfg, p.config)
}

// reloadConfig is the shared execution layer for explicit reload and service
// operations. Keep Caddy's source headers and admin-address resolution; no
// sibling command's flags are mutated or reused.
func reloadConfig(config, adapter, address string, force bool) error {
	body, file, used, err := caddycmd.LoadConfig(config, adapter)
	if err != nil {
		return err
	}
	if file == "" {
		return fmt.Errorf("no config file to load")
	}
	address, err = caddycmd.DetermineAdminAPIAddress(address, body, file, adapter)
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Connection", "close")
	if force {
		headers.Set("Cache-Control", "must-revalidate")
	}
	headers.Set("Caddy-Config-Source-File", file)
	headers.Set("Caddy-Config-Source-Adapter", used)
	resp, err := caddycmd.AdminAPIRequest(address, http.MethodPost, "/load", headers, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sending configuration to instance: %w", err)
	}
	return resp.Body.Close()
}
