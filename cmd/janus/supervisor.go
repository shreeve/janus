package main

// The supervisor's capture of the edge's stdout and stderr. launchd
// (StandardOutPath and StandardErrorPath) and systemd
// (StandardOutput=append: and StandardError=append:) each open that path
// in append mode, creating it when absent, for every start they make, and
// hand the descriptor to the edge. Neither ever rotates it, so the edge
// keeps it bounded itself: at each start under the item it rolls a file
// past supervisorLogMax to one .1 and points its own stdout and stderr at
// a fresh file; the manager's next start opens that fresh file too.
//
// A start refused under the item is retried every ten seconds for as long
// as the cause lasts (a firewall a macOS update took away can last days),
// so each error line carries an RFC 3339 timestamp, and a refusal the
// edge makes before Caddy starts that repeats word for word is written at
// most once per refusalEvery. The state for that is one small file in the
// state directory, removed by the next start that gets past its checks.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	// supervisorLogMax is the size past which a start under the item
	// rolls the supervisor log; one roll (.1) is kept.
	supervisorLogMax = 1 << 20
	// refusalEvery is how often one unchanged refusal is written.
	refusalEvery = 10 * time.Minute
)

// supervisedOutput reports which of this process's stdout and stderr are
// the supervisor log: the item started it. A terminal, a pipe, or Caddy's
// background start is neither.
func supervisedOutput(p servicePaths) (stdout, stderr bool) {
	sup, err := os.Stat(p.sup)
	if err != nil {
		return false, false
	}
	same := func(f *os.File) bool {
		st, err := f.Stat()
		return err == nil && os.SameFile(st, sup)
	}
	return same(os.Stdout), same(os.Stderr)
}

// rotateSupervisorLog rolls the supervisor log to .1 when it is larger
// than max and has reopen point the descriptors that wrote to it (stdout,
// stderr) at a fresh file of the same mode. Reports whether it rolled.
func rotateSupervisorLog(path string, max int64, stdout, stderr bool, reopen func(path string, perm os.FileMode, stdout, stderr bool) error) (bool, error) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= max {
		return false, nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return false, err
	}
	return true, reopen(path, st.Mode().Perm(), stdout, stderr)
}

// stampLines prefixes every line of text with the time, RFC 3339.
func stampLines(now time.Time, text string) string {
	ts := now.Format(time.RFC3339)
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		b.WriteString(ts + " " + line + "\n")
	}
	return b.String()
}

// refusalRecord is the last refusal written to the supervisor log, and how
// many identical ones followed it unwritten.
type refusalRecord struct {
	Message string    `json:"message"`
	Logged  time.Time `json:"logged"`
	Repeats int       `json:"repeats"`
}

func refusalPath(p servicePaths) string { return filepath.Join(p.state, "refusal.json") }

// noteRefusal records a refusal and decides whether it is written now:
// a new message always is, the same one again once refusalEvery has
// passed since it was last written. When it is, prev is the record it
// replaces (its Repeats are the identical refusals that went unwritten).
func noteRefusal(path, msg string, now time.Time) (write bool, prev refusalRecord) {
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &prev)
	}
	rec := refusalRecord{Message: msg, Logged: now}
	if prev.Message == msg && !now.Before(prev.Logged) && now.Sub(prev.Logged) < refusalEvery {
		rec, write = prev, false
		rec.Repeats++
	} else {
		write = true
		if prev.Message != msg {
			prev = refusalRecord{}
		}
	}
	if b, err := json.Marshal(rec); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = writeFileAtomic(path, b, 0o644)
	}
	return write, prev
}

// clearRefusal forgets the last refusal: the edge got past its checks.
func clearRefusal(p servicePaths) { _ = os.Remove(refusalPath(p)) }

// supervisedError writes an error the way the supervisor log keeps it,
// every line stamped, in place of cobra's own unstamped line. A refusal
// (throttle true) that repeats word for word is written at most once per
// refusalEvery, with the count of the ones skipped in between. The error
// is returned as is, so the exit status is unchanged and the manager
// retries.
func supervisedError(cmd *cobra.Command, p servicePaths, err error, throttle bool, now time.Time) error {
	cmd.SilenceErrors = true
	w := cmd.ErrOrStderr()
	msg := "Error: " + err.Error()
	if !throttle {
		_, _ = io.WriteString(w, stampLines(now, msg))
		return err
	}
	write, prev := noteRefusal(refusalPath(p), msg, now)
	if !write {
		return err
	}
	if prev.Repeats > 0 {
		_, _ = io.WriteString(w, stampLines(now, fmt.Sprintf("(the refusal below repeated %d more time%s since %s; an unchanged refusal is written once per %s)",
			prev.Repeats, plural(prev.Repeats), prev.Logged.Format(time.RFC3339), refusalEvery)))
	}
	_, _ = io.WriteString(w, stampLines(now, msg))
	return err
}

// stampedErrorLine matches an error line as supervisedError writes it.
var stampedErrorLine = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d) Error: `)

// lastSupervisorError is the last error line in the supervisor log, with
// its timestamp; an unstamped "Error:" line (an older binary's) when there
// is no stamped one; "" when there is neither.
func lastSupervisorError(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	lines, err := tailLines(f, st.Size(), 200)
	if err != nil {
		return ""
	}
	plain := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if stampedErrorLine.MatchString(line) {
			return line
		}
		if plain == "" && strings.HasPrefix(line, "Error: ") {
			plain = line
		}
	}
	return plain
}
