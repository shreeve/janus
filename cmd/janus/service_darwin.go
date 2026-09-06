package main

// launchd: the edge is a LaunchAgent for a user (a login item in gui/<uid>)
// and a LaunchDaemon for root (the system domain, no session needed).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const defaultLaunchdLabel = "janus.edge"

type launchdItem struct {
	label  string
	domain string
	plist  string
}

func newServiceItem(p servicePaths) serviceItem {
	label := serviceLabel(defaultLaunchdLabel)
	if p.root {
		return &launchdItem{label: label, domain: "system", plist: "/Library/LaunchDaemons/" + label + ".plist"}
	}
	return &launchdItem{
		label:  label,
		domain: "gui/" + strconv.Itoa(os.Getuid()),
		plist:  filepath.Join(p.home, "Library", "LaunchAgents", label+".plist"),
	}
}

func (l *launchdItem) name() string   { return "launchd " + l.target() }
func (l *launchdItem) file() string   { return l.plist }
func (l *launchdItem) target() string { return l.domain + "/" + l.label }

func (l *launchdItem) registered() bool { return fileExists(l.plist) }

var launchdPID = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)

func (l *launchdItem) loaded() (bool, int) {
	out, err := exec.Command("launchctl", "print", l.target()).Output()
	if err != nil {
		return false, 0
	}
	if m := launchdPID.FindSubmatch(out); m != nil {
		pid, _ := strconv.Atoi(string(m[1]))
		return true, pid
	}
	return true, 0
}

func (l *launchdItem) register(p servicePaths, exe string) error {
	if err := os.MkdirAll(filepath.Dir(l.plist), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(l.plist, []byte(launchdPlist(l.label, p, exe)), 0o644); err != nil {
		return err
	}
	// A previous `disable` outlives the plist; clear it so bootstrap and
	// the next boot both take.
	_ = launchctl("enable", l.target())
	return nil
}

func (l *launchdItem) load() error {
	return launchctl("bootstrap", l.domain, l.plist)
}

func (l *launchdItem) kick() error {
	return launchctl("kickstart", l.target())
}

// unregister removes the plist. The loaded job, if any, runs until logout
// or reboot: bootout would kill the edge, and off is not stop.
func (l *launchdItem) unregister() (bool, error) {
	err := os.Remove(l.plist)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("launchctl %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

func launchdPlist(label string, p servicePaths, exe string) string {
	path := os.Getenv("PATH")
	if p.root {
		path = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>run</string>
		<string>--config</string>
		<string>%s</string>
		<string>--adapter</string>
		<string>caddyfile</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%s</string>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, xmlEscape(label), xmlEscape(exe), xmlEscape(p.config), xmlEscape(p.state), xmlEscape(p.home), xmlEscape(path), xmlEscape(p.sup), xmlEscape(p.sup))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// lowPortWarning: on macOS any user may bind 80 and 443.
func lowPortWarning(servicePaths, string) string { return "" }
