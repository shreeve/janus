package main

// Applying and checking the host firewall for a scope: the privileged path.
// Only macOS needs one: a pf anchor wired into /etc/pf.conf, pf enabled by
// reference token, and a LaunchDaemon that enables it again at boot (Apple's
// own job loads the rules at boot but leaves pf disabled). Every write is
// parse-checked first, and a failed install restores what was there.
// Everything goes through fwOps so tests drive it with fakes and this
// host's pf is never touched by a test.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	pfctlBin       = "/sbin/pfctl"
	launchctlBin   = "/bin/launchctl"
	pfConfPath     = "/etc/pf.conf"
	pfAnchorName   = "janus"
	pfAnchorPath   = "/etc/pf.anchors/janus"
	pfDaemonLabel  = "janus.pf"
	pfDaemonPlist  = "/Library/LaunchDaemons/janus.pf.plist"
	pfConfComment  = "# janus exposure mode: the anchor scopes the edge's wildcard socket ('janus firewall')"
	pfConfAnchor   = `anchor "janus"`
	pfConfLoadLine = `load anchor "janus" from "/etc/pf.anchors/janus"`
	pfAppleAnchor  = `anchor "com.apple/*"`
)

// fwOps is the firewall's view of the host.
type fwOps struct {
	goos   string
	run    func(name string, args ...string) ([]byte, error)
	read   func(path string) ([]byte, error)
	write  func(path string, data []byte, perm os.FileMode) error
	remove func(path string) error
}

// hostFwOps is the real host; a variable so the verb tests stand in a fake
// and never read or touch this machine's pf.
var hostFwOps = func() fwOps {
	return fwOps{
		goos:   goosForFirewall(),
		run:    runCmd,
		read:   os.ReadFile,
		write:  writeFileAtomic,
		remove: os.Remove,
	}
}

// writeFileAtomic writes beside the target and renames over it, so a
// reader (pf at boot) never sees a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// firewallVerdict is what a check found.
type firewallVerdict struct {
	needed     bool   // the scope needs a rule on this OS
	verified   bool   // the rule is in force as generated
	unverified bool   // this invocation lacked the privilege to look
	detail     string // why not, when not
}

func (v firewallVerdict) String() string {
	switch {
	case !v.needed:
		return "none (this mode needs no host rule here)"
	case v.verified:
		return "verified"
	case v.unverified:
		return "unverified (run: sudo janus status)"
	}
	return "missing: " + v.detail + " (run: sudo janus firewall)"
}

// applyFirewall installs the rule a scope needs, or removes the rule when
// the scope needs none, then checks it.
func applyFirewall(ops fwOps, st scopeState) (firewallVerdict, error) {
	if ops.goos != "darwin" {
		return firewallVerdict{}, nil // exact bind: nothing to apply
	}
	if err := applyPf(ops, st); err != nil {
		return firewallVerdict{}, err
	}
	v := checkFirewall(ops, st)
	if v.needed && !v.verified {
		return v, fmt.Errorf("the firewall rule did not take: %s", v.detail)
	}
	return v, nil
}

// checkFirewall reports whether the rule a scope needs is in force.
func checkFirewall(ops fwOps, st scopeState) firewallVerdict {
	if !NeedsFirewall(st.Scope, ops.goos) {
		return firewallVerdict{}
	}
	detail := checkPf(ops, st)
	return firewallVerdict{needed: true, verified: detail == "", detail: detail}
}

// --- macOS: pf -----------------------------------------------------------------

func pfctl(ops fwOps, args ...string) ([]byte, error) { return ops.run(pfctlBin, args...) }

// applyPf installs the anchor for a scope. A first install (or a pf.conf
// that lost its janus lines) reloads the main ruleset; otherwise only the
// janus anchor is reloaded, leaving the anchors system services insert at
// runtime alone. A load that fails puts the previous files and the
// previous rules back.
func applyPf(ops fwOps, st scopeState) error {
	if st.Scope == ScopeWAN {
		return removePf(ops)
	}
	anchor, err := PfAnchor(st.Scope, st.onlink())
	if err != nil {
		return err
	}
	prevAnchor, _ := ops.read(pfAnchorPath)
	prevConf, err := ops.read(pfConfPath)
	if err != nil {
		return fmt.Errorf("%s: %v", pfConfPath, err)
	}
	conf, changed := pfConfWithAnchor(string(prevConf), true)
	restore := func() {
		if prevAnchor != nil {
			_ = ops.write(pfAnchorPath, prevAnchor, 0o644)
		} else {
			_ = ops.remove(pfAnchorPath)
		}
		if changed {
			_ = ops.write(pfConfPath, prevConf, 0o644)
		}
	}
	if err := ops.write(pfAnchorPath, []byte(anchor), 0o644); err != nil {
		return err
	}
	if changed {
		if err := ops.write(pfConfPath, []byte(conf), 0o644); err != nil {
			restore()
			return err
		}
		if _, err := pfctl(ops, "-nf", pfConfPath); err != nil {
			restore()
			return fmt.Errorf("pf rules do not parse; nothing changed: %v", err)
		}
		if _, err := pfctl(ops, "-f", pfConfPath); err != nil {
			restore()
			_, _ = pfctl(ops, "-f", pfConfPath)
			return fmt.Errorf("loading pf rules; the previous rules are back: %v", err)
		}
	} else {
		if _, err := pfctl(ops, "-a", pfAnchorName, "-nf", pfAnchorPath); err != nil {
			restore()
			return fmt.Errorf("the janus anchor does not parse; nothing changed: %v", err)
		}
		if _, err := pfctl(ops, "-a", pfAnchorName, "-f", pfAnchorPath); err != nil {
			restore()
			if prevAnchor != nil {
				_, _ = pfctl(ops, "-a", pfAnchorName, "-f", pfAnchorPath)
			} else {
				_, _ = pfctl(ops, "-a", pfAnchorName, "-F", "all")
			}
			return fmt.Errorf("loading the janus anchor; the previous rules are back: %v", err)
		}
	}
	if !pfEnabled(ops) {
		if _, err := pfctl(ops, "-E"); err != nil {
			return fmt.Errorf("enabling pf: %v", err)
		}
	}
	if have, _ := ops.read(pfDaemonPlist); string(have) != pfDaemonPlistBody() {
		if err := ops.write(pfDaemonPlist, []byte(pfDaemonPlistBody()), 0o644); err != nil {
			return err
		}
	}
	if _, err := ops.run(launchctlBin, "print", "system/"+pfDaemonLabel); err != nil {
		if _, err := ops.run(launchctlBin, "bootstrap", "system", pfDaemonPlist); err != nil {
			return fmt.Errorf("installing the boot-time pf enable: %v", err)
		}
	}
	return nil
}

// removePf takes the anchor, its pf.conf lines, and the boot daemon out:
// pf.conf first, so a failure there leaves the rules in force and the
// scope honest. pf's enabled state is left alone: with no janus anchor it
// enforces nothing of ours, and another enabler may hold a token.
func removePf(ops fwOps) error {
	prevConf, err := ops.read(pfConfPath)
	if err != nil {
		return fmt.Errorf("%s: %v", pfConfPath, err)
	}
	if conf, changed := pfConfWithAnchor(string(prevConf), false); changed {
		if err := ops.write(pfConfPath, []byte(conf), 0o644); err != nil {
			return err
		}
		if _, err := pfctl(ops, "-nf", pfConfPath); err != nil {
			_ = ops.write(pfConfPath, prevConf, 0o644)
			return fmt.Errorf("pf rules without the anchor do not parse; %s restored: %v", pfConfPath, err)
		}
		if _, err := pfctl(ops, "-f", pfConfPath); err != nil {
			_ = ops.write(pfConfPath, prevConf, 0o644)
			return fmt.Errorf("loading pf rules; %s restored: %v", pfConfPath, err)
		}
	}
	_, _ = pfctl(ops, "-a", pfAnchorName, "-F", "all")
	if err := ops.remove(pfAnchorPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _ = ops.run(launchctlBin, "bootout", "system/"+pfDaemonLabel)
	if err := ops.remove(pfDaemonPlist); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pfInstalled is the file half of the check, which needs no privilege:
// the anchor file carries the scope's rules, pf.conf loads it, and the
// boot daemon is in place. "" when all three hold, else what is wrong.
func pfInstalled(ops fwOps, st scopeState) string {
	want, err := PfAnchor(st.Scope, st.onlink())
	if err != nil {
		return err.Error()
	}
	if got, err := ops.read(pfAnchorPath); err != nil {
		return pfAnchorPath + " is absent"
	} else if string(got) != want {
		return pfAnchorPath + " does not match the scope"
	}
	if conf, err := ops.read(pfConfPath); err != nil {
		return pfConfPath + ": " + err.Error()
	} else if !pfWired(string(conf)) {
		return pfConfPath + " does not load the janus anchor"
	}
	if have, err := ops.read(pfDaemonPlist); err != nil {
		return pfDaemonPlist + " is absent, so pf would stay disabled after a reboot"
	} else if string(have) != pfDaemonPlistBody() {
		return pfDaemonPlist + " is not the janus daemon"
	}
	return ""
}

// checkPf returns "" when the files are in place, pf is enabled, and the
// rules pf holds in the janus anchor are exactly the anchor file's, else
// what is wrong. pfctl normalizes both sides, so the comparison is exact.
func checkPf(ops fwOps, st scopeState) string {
	if detail := pfInstalled(ops, st); detail != "" {
		return detail
	}
	if !pfEnabled(ops) {
		return "pf is disabled"
	}
	want, err := pfctl(ops, "-a", pfAnchorName, "-nvf", pfAnchorPath)
	if err != nil {
		return "pfctl -nvf: " + err.Error()
	}
	loaded, err := pfctl(ops, "-a", pfAnchorName, "-sr")
	if err != nil {
		return "pfctl -a janus -sr: " + err.Error()
	}
	if !pfRulesEqual(string(loaded), string(want)) {
		return "the rules loaded in the janus anchor are not the anchor file's"
	}
	return ""
}

// pfEnabled asks pf; anything but a clear yes is no.
func pfEnabled(ops fwOps) bool {
	info, err := pfctl(ops, "-s", "info")
	return err == nil && strings.Contains(string(info), "Status: Enabled")
}

// pfRulesEqual compares two pfctl rule listings line by line, in order,
// ignoring blank lines.
func pfRulesEqual(loaded, want string) bool {
	return strings.Join(ruleLines(loaded), "\n") == strings.Join(ruleLines(want), "\n")
}

func ruleLines(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// pfWired reports whether pf.conf declares and loads the janus anchor.
func pfWired(conf string) bool {
	var anchor, load bool
	for _, line := range strings.Split(conf, "\n") {
		switch strings.TrimSpace(line) {
		case pfConfAnchor:
			anchor = true
		case pfConfLoadLine:
			load = true
		}
	}
	return anchor && load
}

// pfPresent reports whether anything of the anchor is on the host: the
// anchor file, or pf.conf lines that load it. Either means a removal has
// work to do (a pf.conf that loads a missing file fails to parse at boot,
// taking Apple's anchor points with it).
func pfPresent(ops fwOps) bool {
	if _, err := ops.read(pfAnchorPath); err == nil {
		return true
	}
	conf, _ := ops.read(pfConfPath)
	return pfWired(string(conf))
}

// pfConfWithAnchor adds or removes the janus lines in pf.conf. The anchor
// goes before Apple's filter anchor: our rules are quick, and a quick match
// in an earlier anchor is final, so nothing a system service loads at
// runtime can pass what the mode blocks. Reports whether anything changed.
func pfConfWithAnchor(conf string, on bool) (string, bool) {
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(conf, "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case pfConfComment, pfConfAnchor, pfConfLoadLine:
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 1 && kept[0] == "" {
		kept = nil
	}
	if on {
		ours := []string{pfConfComment, pfConfAnchor, pfConfLoadLine}
		at := len(kept)
		for i, line := range kept {
			if strings.TrimSpace(line) == pfAppleAnchor {
				at = i
				break
			}
		}
		kept = append(kept[:at], append(ours, kept[at:]...)...)
	}
	out := strings.Join(kept, "\n") + "\n"
	if len(kept) == 0 {
		out = ""
	}
	return out, out != conf
}

// pfDaemonPlistBody is the LaunchDaemon that enables pf at boot. Apple's
// com.apple.pfctl loads /etc/pf.conf at boot but leaves pf disabled; without
// this the anchor would persist and enforce nothing after a restart.
func pfDaemonPlistBody() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + pfDaemonLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/sbin/pfctl</string>
		<string>-E</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
</dict>
</plist>
`
}
