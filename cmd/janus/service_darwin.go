package main

// launchd: the edge is a LaunchAgent for a user (a login item in gui/<uid>)
// and a LaunchDaemon for root (the system domain, no session needed).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
	uid := os.Getuid()
	if p.uid >= 0 {
		uid = p.uid // a verb acting on a user's edge as root
	}
	return &launchdItem{
		label:  label,
		domain: "gui/" + strconv.Itoa(uid),
		plist:  filepath.Join(p.home, "Library", "LaunchAgents", label+".plist"),
	}
}

func (l *launchdItem) name() string   { return "launchd " + l.target() }
func (l *launchdItem) file() string   { return l.plist }
func (l *launchdItem) target() string { return l.domain + "/" + l.label }

func (l *launchdItem) registered() bool { return fileExists(l.plist) }

func (l *launchdItem) loaded() (bool, int) {
	out, err := runOut("launchctl", "print", l.target())
	if err != nil {
		return false, 0
	}
	return true, parseLaunchdPID(out)
}

var launchdPID = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)

// parseLaunchdPID reads the pid from `launchctl print`, present only while
// the job's process runs (absent when "not running" or "spawn scheduled").
func parseLaunchdPID(out []byte) int {
	if m := launchdPID.FindSubmatch(out); m != nil {
		pid, _ := strconv.Atoi(string(m[1]))
		return pid
	}
	return 0
}

func (l *launchdItem) register(p servicePaths, exe string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(l.plist), 0o755); err != nil {
		return false, err
	}
	body := []byte(launchdPlist(l.label, p, exe))
	prev, _ := os.ReadFile(l.plist)
	changed := !bytes.Equal(prev, body)
	if changed {
		if err := os.WriteFile(l.plist, body, 0o644); err != nil {
			return false, err
		}
	}
	// A previous `disable` outlives the plist; clear it so bootstrap and
	// the next login both take.
	_ = launchctl("enable", l.target())
	return changed, nil
}

// load bootstraps the plist. launchd keeps the spec it was last handed,
// so a job it still holds (after a clean stop, or an `autostart off`) is
// booted out first: what runs is what the file says, and a kickstart
// inside ThrottleInterval of the last run is not waited out.
func (l *launchdItem) load() error {
	if loaded, _ := l.loaded(); loaded {
		if err := launchctl("bootout", l.target()); err != nil {
			return err
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if loaded, _ := l.loaded(); !loaded {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return launchctl("bootstrap", l.domain, l.plist)
}

// unregister removes the plist. A loaded job runs on until 'janus stop'
// or logout — bootout would kill the edge, and off is not stop — and
// launchd still revives it after a crash until then.
func (l *launchdItem) unregister() (bool, error) {
	err := os.Remove(l.plist)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func launchctl(args ...string) error {
	_, err := runOut("launchctl", args...)
	return err
}

func launchdPlist(label string, p servicePaths, exe string) string {
	env := serviceEnv(p, exe)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var envXML strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&envXML, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEscape(k), xmlEscape(env[k]))
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
%s	</dict>
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
`, xmlEscape(label), xmlEscape(exe), xmlEscape(p.config), xmlEscape(p.state), envXML.String(), xmlEscape(p.sup), xmlEscape(p.sup))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// platformNotes: on macOS any user may bind 80 and 443, and a LaunchAgent
// needs a GUI session to load into.
func platformNotes(p servicePaths, _ string) []string {
	if p.root {
		return nil
	}
	if os.Getenv("SSH_CONNECTION") != "" {
		return []string{"note: a user's item lives in the GUI login session; from ssh it loads only while that user is logged in at the console (a server wants 'sudo janus autostart')"}
	}
	return nil
}
