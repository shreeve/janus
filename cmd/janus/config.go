package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func setConfig(cmd *cobra.Command, path string) {
	_ = cmd.Flags().Set("config", path)
	if f := cmd.Flags().Lookup("adapter"); f != nil && !f.Changed {
		_ = cmd.Flags().Set("adapter", "caddyfile")
	}
}

// setEnvfile loads the service env file when the command runs the service
// Caddyfile and the operator named no env file of their own.
func setEnvfile(cmd *cobra.Command, p servicePaths) {
	f := cmd.Flags().Lookup("envfile")
	if f == nil || f.Changed || !fileExists(p.env) {
		return
	}
	if usesServiceConfig(cmd, p) {
		_ = cmd.Flags().Set("envfile", p.env)
	}
}

// Every in-process adaptation loads the same environment before deriving the
// reserved bind. Explicit envfiles replace the optional service default.
func prepareConfigEnvironment(cmd *cobra.Command, p servicePaths) (scopeState, error) {
	if usesServiceConfig(cmd, p) {
		if f := cmd.Flags().Lookup("adapter"); f != nil && !f.Changed {
			_ = cmd.Flags().Set("adapter", "caddyfile")
		}
	}
	setEnvfile(cmd, p)
	if cmd.Flags().Lookup("envfile") != nil {
		paths, err := cmd.Flags().GetStringSlice("envfile")
		if err != nil {
			return scopeState{}, err
		}
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				return scopeState{}, err
			}
			if err := loadEnvFile(path); err != nil {
				return scopeState{}, err
			}
		}
	}
	return setBindEnv(p)
}

// targetsService reports whether a stop or reload is aimed at the service
// edge: no explicit address, and an absent or equivalent service config.
func targetsService(cmd *cobra.Command) bool {
	return !cmd.Flags().Changed("address") && (!cmd.Flags().Changed("config") || usesServiceConfig(cmd, currentPaths()))
}

// validateConfig runs this binary's own 'janus validate' on the Caddyfile,
// quietly: Caddy narrates adapting and logger redirection on stderr, which
// only matters when the answer is no. A variable so tests can validate
// in-process (a test binary re-executed as 'validate' runs the tests).
var validateConfig = func(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := exec.Command(exe, "validate", "--config", path, "--adapter", "caddyfile").CombinedOutput()
	if err != nil {
		// Keep the verdict, drop the narration and the redundant prefix.
		var keep []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(line, "{") {
				continue
			}
			keep = append(keep, strings.TrimPrefix(line, "Error: "))
		}
		if len(keep) == 0 {
			keep = []string{err.Error()}
		}
		return errors.New(strings.Join(keep, "\n"))
	}
	return nil
}

// validateInProcess is what 'janus validate' does, without the process.
func validateInProcess(path string) error {
	if sameConfigPath(path, currentPaths().config) {
		if err := loadEnvFile(currentPaths().env); err != nil {
			return err
		}
	}
	if _, err := setBindEnv(currentPaths()); err != nil {
		return err
	}
	cfgJSON, _, _, err := caddycmd.LoadConfig(path, "caddyfile")
	if err != nil {
		return err
	}
	var cfg caddy.Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return err
	}
	return caddy.Validate(&cfg)
}
