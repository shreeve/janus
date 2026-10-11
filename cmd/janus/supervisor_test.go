package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

var testNow = time.Date(2026, 10, 10, 9, 30, 0, 0, time.FixedZone("EDT", -4*3600))

// Every line of an error is stamped with the time, RFC 3339.
func TestStampLines(t *testing.T) {
	got := stampLines(testNow, "Error: one\ntwo\n")
	want := "2026-10-10T09:30:00-04:00 Error: one\n2026-10-10T09:30:00-04:00 two\n"
	if got != want {
		t.Errorf("stampLines = %q, want %q", got, want)
	}
	if !stampedErrorLine.MatchString(strings.Split(got, "\n")[0]) {
		t.Error("the stamped error line does not match what status looks for")
	}
}

// Only a file past the limit rolls: to one .1 (replacing an older .1),
// and the descriptors that wrote to it are pointed at a fresh file of the
// same mode.
func TestRotateSupervisorLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor.log")
	if err := os.WriteFile(path, []byte("small\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var calls []string
	reopen := func(p string, perm os.FileMode, stdout, stderr bool) error {
		calls = append(calls, p)
		if perm != 0o640 || !stdout || !stderr {
			t.Errorf("reopen %s perm %v stdout %v stderr %v", p, perm, stdout, stderr)
		}
		return os.WriteFile(p, nil, perm)
	}
	if rolled, err := rotateSupervisorLog(path, 16, true, true, reopen); rolled || err != nil || len(calls) != 0 {
		t.Fatalf("a small log rolled: %v %v %v", rolled, err, calls)
	}
	if err := os.WriteFile(path+".1", []byte("older roll\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("Error: the host firewall for lan is not in place\n"), 4)
	if err := os.WriteFile(path, big, 0o640); err != nil {
		t.Fatal(err)
	}
	if rolled, err := rotateSupervisorLog(path, 16, true, true, reopen); !rolled || err != nil {
		t.Fatalf("a large log did not roll: %v %v", rolled, err)
	}
	if got, _ := os.ReadFile(path + ".1"); !bytes.Equal(got, big) {
		t.Errorf(".1 holds %q", got)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 || len(calls) != 1 || calls[0] != path {
		t.Errorf("fresh log: %v %v %v", st, err, calls)
	}
	if rolled, _ := rotateSupervisorLog(filepath.Join(t.TempDir(), "absent.log"), 16, false, true, reopen); rolled {
		t.Error("an absent log rolled")
	}
}

// The edge knows it runs under the item when its stderr is the
// supervisor log; a terminal or a pipe is not.
func TestSupervisedOutput(t *testing.T) {
	p := servicePathsFor(false, t.TempDir(), func(string) string { return "" })
	if _, e := supervisedOutput(p); e {
		t.Fatal("supervised without a supervisor log")
	}
	if err := os.MkdirAll(filepath.Dir(p.sup), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p.sup, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, e := supervisedOutput(p); e {
		t.Fatal("this test's stderr passed for the supervisor log")
	}
	prev := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = prev }()
	if o, e := supervisedOutput(p); !e || o {
		t.Errorf("stderr on the supervisor log: stdout %v stderr %v", o, e)
	}
}

// An unchanged refusal is written once per refusalEvery; the next write
// carries the count of the skipped ones. A different refusal is written
// at once.
func TestNoteRefusal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "refusal.json")
	const msg = "Error: the host firewall for lan is not in place"
	if write, _ := noteRefusal(path, msg, testNow); !write {
		t.Fatal("a first refusal was not written")
	}
	for i := 1; i <= 3; i++ {
		if write, _ := noteRefusal(path, msg, testNow.Add(time.Duration(i)*10*time.Second)); write {
			t.Fatalf("repeat %d was written", i)
		}
	}
	write, prev := noteRefusal(path, msg, testNow.Add(refusalEvery))
	if !write || prev.Repeats != 3 || !prev.Logged.Equal(testNow) {
		t.Fatalf("after the window: write %v prev %+v", write, prev)
	}
	if write, prev := noteRefusal(path, "Error: another", testNow.Add(refusalEvery+time.Second)); !write || prev.Repeats != 0 {
		t.Errorf("a new refusal: write %v prev %+v", write, prev)
	}
	// A clock that went back is not a reason to stay quiet.
	if write, _ := noteRefusal(path, "Error: another", testNow); !write {
		t.Error("a refusal stamped before the last write was swallowed")
	}
}

// Under the item, an error is written stamped in place of cobra's line,
// and returned unchanged for the exit status; a repeated refusal writes
// nothing until its window passes.
func TestSupervisedError(t *testing.T) {
	p := servicePathsFor(false, t.TempDir(), func(string) string { return "" })
	var buf bytes.Buffer
	cmd := &cobra.Command{Use: "run"}
	cmd.SetErr(&buf)
	refusal := errors.New("the host firewall for lan is not in place (/etc/pf.conf does not load the janus anchor); Run: sudo janus firewall")
	if err := supervisedError(cmd, p, refusal, true, testNow); err != refusal || !cmd.SilenceErrors {
		t.Fatalf("returned %v, silenced %v", err, cmd.SilenceErrors)
	}
	first := buf.String()
	if first != "2026-10-10T09:30:00-04:00 Error: "+refusal.Error()+"\n" {
		t.Errorf("written %q", first)
	}
	_ = supervisedError(cmd, p, refusal, true, testNow.Add(10*time.Second))
	if buf.String() != first {
		t.Errorf("a repeat within the window was written: %q", buf.String()[len(first):])
	}
	_ = supervisedError(cmd, p, refusal, true, testNow.Add(refusalEvery))
	if rest := buf.String()[len(first):]; !strings.Contains(rest, "repeated 1 more time since 2026-10-10T09:30:00-04:00") || !strings.Contains(rest, "Error: "+refusal.Error()) {
		t.Errorf("after the window: %q", rest)
	}
	// Caddy's own failures are never held back.
	buf.Reset()
	caddyErr := errors.New("loading initial config: bind: address in use")
	_ = supervisedError(cmd, p, caddyErr, false, testNow)
	_ = supervisedError(cmd, p, caddyErr, false, testNow)
	if strings.Count(buf.String(), "Error: loading initial config") != 2 {
		t.Errorf("unthrottled errors: %q", buf.String())
	}
	clearRefusal(p)
	if _, err := os.Stat(refusalPath(p)); !os.IsNotExist(err) {
		t.Error("clearRefusal left the state file")
	}
}

// status shows the last stamped error line, or an older binary's
// unstamped one when there is none.
func TestLastSupervisorError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.log")
	if lastSupervisorError(path) != "" {
		t.Error("an absent log has an error")
	}
	log := strings.Join([]string{
		"Error: an older binary's refusal",
		"2026-10-10T09:00:00-04:00 Error: first refusal",
		"2026-10-10T09:00:00-04:00 second line of it",
		`{"level":"info","msg":"serving initial configuration"}`,
		"2026-10-10T09:10:00Z Error: the host firewall for lan is not in place",
		`{"level":"info","msg":"shutting down"}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastSupervisorError(path); got != "2026-10-10T09:10:00Z Error: the host firewall for lan is not in place" {
		t.Errorf("last error %q", got)
	}
	if err := os.WriteFile(path, []byte("Error: one\nError: two\nplain\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lastSupervisorError(path); got != "Error: two" {
		t.Errorf("unstamped fallback %q", got)
	}
}

// exitItem is a fake item whose manager reports a last exit status.
type exitItem struct {
	*fakeItem
	code  int
	known bool
}

func (e exitItem) lastExit() (int, bool) { return e.code, e.known }

// A job the manager holds with no edge running, whose last process
// failed, is a crash loop: status says so and shows why. A clean stop
// shows neither.
func TestStatusCrashLoop(t *testing.T) {
	p := isolatedHome(t)
	if err := os.MkdirAll(filepath.Dir(p.sup), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "2026-10-10T09:10:00-04:00 Error: the host firewall for lan is not in place (/etc/pf.conf does not load the janus anchor); the edge refuses to open a wildcard socket unscoped. Run: sudo janus firewall"
	if err := os.WriteFile(p.sup, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := exitItem{fakeItem: &fakeItem{reg: true, isLoaded: true}, code: 1, known: true}
	prev := itemFor
	itemFor = func(servicePaths) serviceItem { return item }
	t.Cleanup(func() { itemFor = prev })
	st := gatherStatus(p)
	if !st.CrashLoop || st.LastExit == nil || *st.LastExit != 1 || st.LastError != line {
		t.Fatalf("crash loop: %+v", st)
	}
	var out bytes.Buffer
	printStatus(&out, p, st)
	for _, want := range []string{"edge     CRASH LOOP: fake keeps starting it and it exits (status 1)", "error    " + line} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
	item.code = 0
	itemFor = func(servicePaths) serviceItem { return item }
	if st := gatherStatus(p); st.CrashLoop || st.LastError != "" {
		t.Errorf("a clean stop reported as a crash loop: %+v", st)
	}
	// A manager that cannot say how the job ended: the line still shows.
	itemFor = func(servicePaths) serviceItem { return item.fakeItem }
	if st := gatherStatus(p); st.CrashLoop || st.LastError != line {
		t.Errorf("unknown exit: %+v", st)
	}
}
