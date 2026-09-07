package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHost records what the firewall code would do to a host. Its pfctl
// parses the way the real one does as far as the tests care: a load of a
// file that is not there fails, and -nvf normalizes the anchor file.
type fakeHost struct {
	files     map[string]string
	cmds      []string
	fail      map[string]string // command prefix -> error text
	out       map[string]string // command prefix -> stdout
	failWrite map[string]bool
}

func newFakeHost(goos string) (*fakeHost, fwOps) {
	h := &fakeHost{files: map[string]string{}, fail: map[string]string{}, out: map[string]string{}, failWrite: map[string]bool{}}
	ops := fwOps{
		goos: goos,
		run: func(name string, args ...string) ([]byte, error) {
			line := strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " "))
			h.cmds = append(h.cmds, line)
			for prefix, msg := range h.fail {
				if strings.HasPrefix(line, prefix) {
					return nil, errors.New(msg)
				}
			}
			switch {
			case strings.HasPrefix(line, "pfctl -nf ") || strings.HasPrefix(line, "pfctl -f "):
				if _, ok := h.files[args[len(args)-1]]; !ok {
					return nil, errors.New("no such file")
				}
				if _, ok := h.files[pfAnchorPath]; strings.HasSuffix(line, pfConfPath) && pfWired(h.files[pfConfPath]) && !ok {
					return nil, errors.New("pf.conf loads an anchor file that is not there")
				}
			case strings.HasPrefix(line, "pfctl -a janus -nvf ") || strings.HasPrefix(line, "pfctl -a janus -nf ") || strings.HasPrefix(line, "pfctl -a janus -f "):
				body, ok := h.files[args[len(args)-1]]
				if !ok {
					return nil, errors.New("no such file")
				}
				if strings.Contains(line, "-nvf") {
					return []byte(normalizedPf(body)), nil
				}
			case line == "pfctl -E":
				h.out["pfctl -s info"] = "Status: Enabled for 0 days 00:00:01\n"
			}
			for prefix, out := range h.out {
				if strings.HasPrefix(line, prefix) {
					return []byte(out), nil
				}
			}
			return nil, nil
		},
		read: func(path string) ([]byte, error) {
			s, ok := h.files[path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return []byte(s), nil
		},
		write: func(path string, data []byte, _ os.FileMode) error {
			if h.failWrite[path] {
				return errors.New("read-only file system")
			}
			h.files[path] = string(data)
			return nil
		},
		remove: func(path string) error { delete(h.files, path); return nil },
	}
	return h, ops
}

func (h *fakeHost) ran(prefix string) bool { return h.index(prefix) >= 0 }

func (h *fakeHost) index(prefix string) int {
	for i, c := range h.cmds {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

// installed puts the files for a scope in place, as 'janus firewall' leaves
// them, without pf holding anything yet.
func (h *fakeHost) installed(st scopeState) {
	h.files[pfAnchorPath], _ = PfAnchor(st.Scope, st.onlink())
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	h.files[pfDaemonPlist] = pfDaemonPlistBody()
}

// bare is a host nothing of janus has touched.
func (h *fakeHost) bare() {
	h.files = map[string]string{pfConfPath: applePfConf}
	h.out = map[string]string{}
}

// inForce sets pf's enabled state and the loaded anchor to what the anchor
// file says: a host on which the rule is in force.
func (h *fakeHost) inForce() {
	h.out["pfctl -s info"] = "Status: Enabled for 0 days 00:01:02           Debug: Urgent\n"
	h.out["pfctl -a janus -sr"] = normalizedPf(h.files[pfAnchorPath])
}

const applePfConf = `#
# Default PF configuration file.
#
scrub-anchor "com.apple/*"
nat-anchor "com.apple/*"
rdr-anchor "com.apple/*"
dummynet-anchor "com.apple/*"
anchor "com.apple/*"
load anchor "com.apple" from "/etc/pf.anchors/com.apple"
`

func lanState() scopeState {
	return scopeState{Scope: ScopeLAN, Interface: "en0", LANV4: "10.0.0.211", OnLinkV4: "10.0.0.0/24"}
}

// normalizedPf is the anchor the way pfctl prints it: one rule per source
// and port, "no state" for stateless, "block drop" for block.
func normalizedPf(anchor string) string {
	var b strings.Builder
	for _, line := range strings.Split(anchor, "\n") {
		if !strings.HasPrefix(line, "pass") && !strings.HasPrefix(line, "block") {
			continue
		}
		head, rest, _ := strings.Cut(line, " from ")
		srcs, rest, _ := strings.Cut(rest, " to ")
		dst, ports, _ := strings.Cut(rest, " port ")
		ports, tail, _ := strings.Cut(ports, "}")
		tail = strings.TrimSpace(strings.ReplaceAll(tail, "flags any", ""))
		head = strings.Replace(head, "block in", "block drop in", 1)
		for _, src := range strings.Fields(strings.Trim(srcs, "{} ")) {
			for _, port := range strings.Fields(strings.Trim(ports, "{ ")) {
				b.WriteString(head + " from " + src + " to " + dst + " port = " + port)
				if tail != "" {
					b.WriteString(" " + tail)
				}
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// The janus lines go before Apple's filter anchor (quick rules in an
// earlier anchor are final); adding is idempotent and removal restores
// Apple's file exactly.
func TestPfConfWithAnchor(t *testing.T) {
	on, changed := pfConfWithAnchor(applePfConf, true)
	if !changed || !pfWired(on) {
		t.Fatalf("anchor lines not added:\n%s", on)
	}
	if strings.Index(on, pfConfLoadLine) > strings.Index(on, "\n"+pfAppleAnchor) || !strings.Contains(on, `dummynet-anchor "com.apple/*"`+"\n"+pfConfComment) {
		t.Errorf("janus lines not directly before Apple's filter anchor:\n%s", on)
	}
	if again, changed := pfConfWithAnchor(on, true); changed || again != on {
		t.Error("adding twice changed the file")
	}
	off, changed := pfConfWithAnchor(on, false)
	if !changed || off != applePfConf || pfWired(off) {
		t.Errorf("removal did not restore Apple's file:\n%s", off)
	}
	if _, changed := pfConfWithAnchor(applePfConf, false); changed {
		t.Error("removing from a clean file changed it")
	}
	// A file without Apple's anchor gets the lines at the end.
	on, _ = pfConfWithAnchor("set skip on lo0\n", true)
	if !strings.HasSuffix(on, pfConfLoadLine+"\n") || !strings.HasPrefix(on, "set skip on lo0\n") {
		t.Errorf("append without Apple's anchor:\n%s", on)
	}
}

// First install for lan: anchor written, pf.conf wired, the main ruleset
// parse-checked then loaded, pf enabled by token because it was off, boot
// daemon installed, then verified against what pfctl reports.
func TestApplyPfLAN(t *testing.T) {
	h, ops := newFakeHost("darwin")
	h.files[pfConfPath] = applePfConf
	h.out["pfctl -s info"] = "Status: Disabled\n"
	h.fail["launchctl print system/janus.pf"] = "Could not find service"
	prevRun := ops.run
	ops.run = func(name string, args ...string) ([]byte, error) {
		out, err := prevRun(name, args...)
		if strings.Join(args, " ") == "-f "+pfConfPath {
			h.out["pfctl -a janus -sr"] = normalizedPf(h.files[pfAnchorPath])
		}
		return out, err
	}
	v, err := applyFirewall(ops, lanState())
	if err != nil {
		t.Fatalf("apply: %v\ncmds: %v", err, h.cmds)
	}
	if !v.verified || v.String() != "verified" {
		t.Errorf("verdict %+v", v)
	}
	want, _ := PfAnchor(ScopeLAN, lanState().onlink())
	if h.files[pfAnchorPath] != want || !pfWired(h.files[pfConfPath]) || h.files[pfDaemonPlist] != pfDaemonPlistBody() {
		t.Errorf("files: %v", h.files)
	}
	last := -1
	for _, step := range []string{"pfctl -nf /etc/pf.conf", "pfctl -f /etc/pf.conf", "pfctl -E", "launchctl bootstrap system " + pfDaemonPlist} {
		idx := h.index(step)
		if idx < 0 || idx < last {
			t.Errorf("step %q missing or out of order in %v", step, h.cmds)
		}
		last = idx
	}
}

// With pf.conf already wired, only the janus anchor is reloaded: the main
// ruleset (and the anchors system services insert at runtime) is left
// alone, and pf is not enabled again.
func TestApplyPfReloadsAnchorOnly(t *testing.T) {
	h, ops := newFakeHost("darwin")
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	h.files[pfAnchorPath], _ = PfAnchor(ScopeLocalhost, lanState().onlink())
	h.files[pfDaemonPlist] = pfDaemonPlistBody()
	h.inForce()
	prevRun := ops.run
	ops.run = func(name string, args ...string) ([]byte, error) {
		out, err := prevRun(name, args...)
		if strings.Join(args, " ") == "-a janus -f "+pfAnchorPath {
			h.out["pfctl -a janus -sr"] = normalizedPf(h.files[pfAnchorPath])
		}
		return out, err
	}
	if _, err := applyFirewall(ops, lanState()); err != nil {
		t.Fatalf("apply: %v\ncmds: %v", err, h.cmds)
	}
	if h.ran("pfctl -f /etc/pf.conf") || h.ran("pfctl -nf /etc/pf.conf") || h.ran("pfctl -E") || h.ran("launchctl bootstrap") {
		t.Errorf("a re-apply touched the main ruleset or pf's token: %v", h.cmds)
	}
	if h.index("pfctl -a janus -nf") < 0 || h.index("pfctl -a janus -nf") > h.index("pfctl -a janus -f ") {
		t.Errorf("anchor not parse-checked before loading: %v", h.cmds)
	}
}

// A parse failure leaves the host exactly as found.
func TestApplyPfParseFailureRestores(t *testing.T) {
	h, ops := newFakeHost("darwin")
	h.files[pfConfPath] = applePfConf
	h.fail["pfctl -nf"] = "syntax error"
	if _, err := applyFirewall(ops, scopeState{Scope: ScopeLocalhost}); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("parse failure: %v", err)
	}
	if h.files[pfConfPath] != applePfConf {
		t.Error("pf.conf not restored")
	}
	if _, ok := h.files[pfAnchorPath]; ok {
		t.Error("anchor file left behind")
	}
	if h.ran("pfctl -f ") || h.ran("pfctl -E") {
		t.Errorf("rules loaded after a parse failure: %v", h.cmds)
	}
}

// A load that fails after parsing puts the previous rules back in pf, not
// just the previous files.
func TestApplyPfLoadFailureReloadsPrevious(t *testing.T) {
	h, ops := newFakeHost("darwin")
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	prevAnchor, _ := PfAnchor(ScopeLocalhost, lanState().onlink())
	h.files[pfAnchorPath] = prevAnchor
	h.inForce()
	loads := 0
	prevRun := ops.run
	ops.run = func(name string, args ...string) ([]byte, error) {
		if strings.Join(args, " ") == "-a janus -f "+pfAnchorPath {
			loads++
			if loads == 1 {
				return nil, errors.New("pfctl: DIOCADDRULE: Operation not permitted")
			}
		}
		return prevRun(name, args...)
	}
	if _, err := applyFirewall(ops, lanState()); err == nil || !strings.Contains(err.Error(), "previous rules are back") {
		t.Fatalf("load failure: %v", err)
	}
	if h.files[pfAnchorPath] != prevAnchor || loads != 2 {
		t.Errorf("previous anchor not restored and reloaded: loads=%d files=%v", loads, h.files)
	}
}

// wan takes everything out again, pf.conf first, and leaves pf's enabled
// state alone.
func TestApplyPfWANRemoves(t *testing.T) {
	h, ops := newFakeHost("darwin")
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	h.files[pfAnchorPath] = "old"
	h.files[pfDaemonPlist] = "old"
	v, err := applyFirewall(ops, scopeState{Scope: ScopeWAN})
	if err != nil || v.needed {
		t.Fatalf("wan: %+v %v", v, err)
	}
	if h.files[pfConfPath] != applePfConf {
		t.Error("pf.conf still wired")
	}
	for _, f := range []string{pfAnchorPath, pfDaemonPlist} {
		if _, ok := h.files[f]; ok {
			t.Errorf("%s left behind", f)
		}
	}
	if !h.ran("pfctl -a janus -F all") || !h.ran("launchctl bootout system/janus.pf") || h.ran("pfctl -d") || h.ran("pfctl -X") {
		t.Errorf("cmds: %v", h.cmds)
	}
	if h.index("pfctl -f /etc/pf.conf") > h.index("pfctl -a janus -F all") {
		t.Errorf("anchor flushed before pf.conf stopped loading it: %v", h.cmds)
	}
	// When pf.conf cannot be rewritten, the rules stay in force and the
	// anchor file stays, so nothing claims a removal that did not happen.
	h, ops = newFakeHost("darwin")
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	h.files[pfAnchorPath] = "old"
	h.failWrite[pfConfPath] = true
	if _, err := applyFirewall(ops, scopeState{Scope: ScopeWAN}); err == nil {
		t.Fatal("removal reported success with pf.conf unwritable")
	}
	if h.files[pfAnchorPath] != "old" || h.ran("pfctl -a janus -F all") {
		t.Errorf("host opened before the removal was complete: %v %v", h.files, h.cmds)
	}
}

// The check names what is missing, in the order an operator would fix it,
// and the loaded rules must be the anchor file's exactly.
func TestCheckPfVerdicts(t *testing.T) {
	h, ops := newFakeHost("darwin")
	st := lanState()
	want, _ := PfAnchor(ScopeLAN, st.onlink())
	expect := func(detail string) {
		t.Helper()
		v := checkFirewall(ops, st)
		if v.verified != (detail == "") || !strings.Contains(v.detail, detail) {
			t.Errorf("want %q, got %+v", detail, v)
		}
	}
	expect(pfAnchorPath + " is absent")
	h.files[pfAnchorPath] = "stale"
	expect("does not match the scope")
	h.files[pfAnchorPath] = want
	h.files[pfConfPath] = applePfConf
	expect("does not load the janus anchor")
	h.files[pfConfPath], _ = pfConfWithAnchor(applePfConf, true)
	expect(pfDaemonPlist + " is absent")
	h.files[pfDaemonPlist] = "something else"
	expect("is not the janus daemon")
	h.files[pfDaemonPlist] = pfDaemonPlistBody()
	if len(h.cmds) != 0 {
		t.Errorf("the file half ran commands: %v", h.cmds)
	}
	h.out["pfctl -s info"] = "Status: Disabled\n"
	expect("pf is disabled")
	h.out["pfctl -s info"] = "Status: Enabled for 0 days\n"
	h.out["pfctl -a janus -sr"] = ""
	expect("not the anchor file's")
	loaded := normalizedPf(want)
	h.out["pfctl -a janus -sr"] = strings.ReplaceAll(loaded, "from 10.0.0.0/24", "from 10.0.9.0/24")
	expect("not the anchor file's")
	h.out["pfctl -a janus -sr"] = loaded + "pass in quick proto tcp from any to any port = 443 no state\n"
	expect("not the anchor file's")
	lines := strings.SplitAfter(strings.TrimSpace(loaded), "\n")
	h.out["pfctl -a janus -sr"] = strings.Join(append(lines[len(lines)-2:], lines[:len(lines)-2]...), "")
	expect("not the anchor file's")
	h.out["pfctl -a janus -sr"] = loaded
	expect("")
	if !strings.Contains(checkFirewall(ops, scopeState{Scope: ScopeWAN}).String(), "none") {
		t.Error("wan needs no rule")
	}
}

// The real pfctl parses what PfAnchor writes: the passes stateless, the
// block on every destination. Parse only (-n): nothing is loaded.
func TestPfAnchorParsesOnHost(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("pfctl is macOS's")
	}
	if _, err := os.Stat(pfctlBin); err != nil {
		t.Skip(err)
	}
	for _, scope := range []Scope{ScopeLocalhost, ScopeLAN} {
		anchor, _ := PfAnchor(scope, lanState().onlink())
		path := filepath.Join(t.TempDir(), "anchor")
		if err := os.WriteFile(path, []byte(anchor), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(pfctlBin, "-a", pfAnchorName, "-nvf", path).Output()
		if err != nil {
			t.Fatalf("%s: pfctl -nvf: %v\n%s", scope, err, anchor)
		}
		got := string(out)
		if !strings.Contains(got, "block drop in quick proto tcp from any to any port = 443") || !strings.Contains(got, "from 127.0.0.0/8 to any port = 80 no state") || strings.Contains(got, "keep state") {
			t.Errorf("%s: pfctl normalized the anchor unexpectedly:\n%s", scope, got)
		}
	}
}

// Linux binds exact addresses: nothing to apply or check, on any scope.
func TestApplyFirewallLinuxIsNone(t *testing.T) {
	h, ops := newFakeHost("linux")
	for _, st := range []scopeState{lanState(), {Scope: ScopeLocalhost}, {Scope: ScopeWAN}} {
		v, err := applyFirewall(ops, st)
		if err != nil || v.needed || !strings.Contains(v.String(), "none") {
			t.Errorf("%s: %+v %v", st.Scope, v, err)
		}
	}
	if len(h.cmds) != 0 || len(h.files) != 0 {
		t.Errorf("linux touched the host: %v %v", h.cmds, h.files)
	}
}

// Off macOS the host firewall is never Janus's to touch (Windows' Defender
// allow rules land with Windows support).
func TestApplyFirewallOffDarwinIsNone(t *testing.T) {
	h, ops := newFakeHost("windows")
	if v, err := applyFirewall(ops, lanState()); err != nil || v.needed || len(h.cmds) != 0 {
		t.Errorf("windows: %+v %v %v", v, err, h.cmds)
	}
}
