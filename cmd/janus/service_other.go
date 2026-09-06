//go:build !darwin && !linux

package main

// No supervisor here: start/stop/restart/status still work through Caddy's
// pidfile start, and autostart says so.

func newServiceItem(servicePaths) serviceItem { return nil }

func lowPortWarning(servicePaths, string) string { return "" }
