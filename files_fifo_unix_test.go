//go:build !windows

package janus

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestFilesRejectFIFOsWithoutWaitingForWriter(t *testing.T) {
	for _, kind := range []string{"canonical", "index", "shell", "sidecar"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name, path, shell := "pipe", "/pipe", ""
			switch kind {
			case "index":
				name, path = "index.html", "/"
			case "shell":
				name, path, shell = "shell.html", "/missing", filepath.Join(dir, "shell.html")
			case "sidecar":
				name, path = "plain.txt.br", "/plain.txt"
				if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("identity"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			pipe := filepath.Join(dir, name)
			if err := unix.Mkfifo(pipe, 0600); err != nil {
				t.Fatal(err)
			}
			h := newBrowseTestHandler(t, nil)
			r := httptest.NewRequest("GET", "https://example.test"+path, nil)
			r.Header.Set("Accept", "text/html")
			r.Header.Set("Accept-Encoding", "br")
			w := httptest.NewRecorder()
			done := make(chan bool, 1)
			go func() {
				done <- h.serveBrowseRootsPrecompressed(w, r, path, []activeBrowseRoot{{path: dir, browse: true}}, shell, true, []string{"br"})
			}()
			select {
			case handled := <-done:
				if (kind == "canonical" || kind == "shell") && handled {
					t.Fatal("served a FIFO")
				}
				if kind == "sidecar" && (!handled || w.Body.String() != "identity" || w.Header().Get("Content-Encoding") != "") {
					t.Fatalf("invalid sidecar fallback: %v %v %s", handled, w.Header(), w.Body.String())
				}
				if kind == "index" && (!handled || w.Code != 200) {
					t.Fatalf("index did not fall back to listing: %v %d", handled, w.Code)
				}
			case <-time.After(time.Second):
				// Release a regressed blocking open so the test never leaks its goroutine.
				fd, err := unix.Open(pipe, unix.O_RDWR|unix.O_NONBLOCK, 0)
				if err == nil {
					<-done
					unix.Close(fd)
				}
				t.Fatal("FIFO blocked the request waiting for a writer")
			}
		})
	}
}

func TestOpenRootFileConfinesSymlinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if f, err := openRootFile(root, "escape"); err == nil {
		f.Close()
		t.Fatal("opened a symlink outside the root")
	}
}
