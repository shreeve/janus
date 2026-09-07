package main

// The edge's log, where an operator looks first: the last lines, or a
// follow that survives the file rolling over (the seed rolls it at 10 MiB).
// When the edge never wrote a line of its own — it refused to start, or
// the supervisor could not run it — the supervisor's capture of its
// stdout and stderr is where the reason is, so that file is read instead.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func logsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "logs [-n <lines>] [-f]",
		Long: `
The edge's log (the file the service Caddyfile writes): the last lines,
then every line as it arrives, across roll-overs, until Ctrl-C. Piped or
redirected, it prints the last lines and exits; -f follows anyway.

The lines are Caddy's JSON, one per line, as written. --supervisor reads
the supervisor's capture of the edge's stdout and stderr instead, which is
where a start that failed before the log opened left its reason; that file
is also what prints when the edge's own log does not exist yet.
`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			n, _ := cmd.Flags().GetInt("lines")
			follow, _ := cmd.Flags().GetBool("follow")
			if !cmd.Flags().Changed("follow") {
				follow = stdoutIsTerminal()
			}
			supervisor, _ := cmd.Flags().GetBool("supervisor")
			p, note := delegatedPaths()
			path := p.log
			if supervisor || !fileExists(p.log) {
				path = p.sup
				if !supervisor {
					note = strings.TrimSpace(note + "\nthe edge has not written " + p.log + " yet; this is the supervisor's capture, " + p.sup)
				}
			}
			if !fileExists(path) {
				return fmt.Errorf("no log at %s; the edge has not run here ('janus autostart' installs it)", path)
			}
			if note != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), note)
			}
			out := cmd.OutOrStdout()
			offset, err := printTail(out, path, n)
			if err != nil || !follow {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			return followFile(ctx, out, path, offset)
		},
	}
	cmd.Flags().IntP("lines", "n", 50, "How many trailing lines to print first")
	cmd.Flags().BoolP("follow", "f", false, "Follow even when piped (on a terminal the log is always followed)")
	cmd.Flags().Bool("supervisor", false, "Read the supervisor's capture of stdout and stderr instead")
	return cmd
}

func stdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// printTail writes the last n lines of the file and returns the offset
// at which a follow continues (the end of the file as it was read).
func printTail(w io.Writer, path string, n int) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	lines, err := tailLines(f, size, n)
	if err != nil {
		return 0, err
	}
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	return size, nil
}

// tailLines reads the last n complete lines of an open file of the given
// size, scanning back from the end in chunks so a large log is not read
// whole.
func tailLines(f io.ReaderAt, size int64, n int) ([]string, error) {
	if n <= 0 || size == 0 {
		return nil, nil
	}
	const chunk = 64 << 10
	var buf []byte
	end := size
	for end > 0 && bytes.Count(buf, []byte{'\n'}) <= n {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		part := make([]byte, end-start)
		if _, err := f.ReadAt(part, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		buf = append(part, buf...)
		end = start
	}
	text := strings.TrimRight(string(buf), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// followFile prints lines appended after offset until the context ends.
// A file that shrank or was replaced (the log rolled) is reopened from
// its start, so nothing after a roll-over is missed.
func followFile(ctx context.Context, w io.Writer, path string, offset int64) error {
	var (
		f       *os.File
		partial []byte
		ident   os.FileInfo
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	open := func() error {
		if f != nil {
			f.Close()
		}
		var err error
		if f, err = os.Open(path); err != nil {
			return err
		}
		ident, err = f.Stat()
		if err != nil {
			return err
		}
		_, err = f.Seek(offset, io.SeekStart)
		return err
	}
	if err := open(); err != nil {
		return err
	}
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		buf, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		if len(buf) > 0 {
			offset += int64(len(buf))
			partial = append(partial, buf...)
			if i := bytes.LastIndexByte(partial, '\n'); i >= 0 {
				_, _ = w.Write(partial[:i+1])
				partial = append([]byte(nil), partial[i+1:]...)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		// Rolled: a new inode at the path, or the file is shorter than
		// what was already read.
		now, err := os.Stat(path)
		if err != nil {
			continue // between the rename and the new file; try again
		}
		if !os.SameFile(now, ident) || now.Size() < offset {
			offset, partial = 0, nil
			if err := open(); err != nil {
				continue
			}
		}
	}
}
