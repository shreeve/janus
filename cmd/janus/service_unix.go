//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// A root service's executable, symlinks, and traversed directories must
// all be controlled by root. Resolve links one component at a time: '..'
// after a symlink applies to its target, not to its lexical parent.
func rootOwnedAndPrivate(path string) bool {
	return protectedExecutablePath(path, func(st os.FileInfo) bool {
		sys, ok := st.Sys().(*syscall.Stat_t)
		return ok && sys.Uid == 0 && (st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0o022 == 0)
	})
}

func protectedExecutablePath(path string, protected func(os.FileInfo) bool) bool {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		path = wd + "/" + path
	}
	root, err := os.Lstat("/")
	if err != nil || !protected(root) {
		return false
	}
	current := "/"
	pending := strings.Split(path, "/")
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		next := filepath.Join(current, part)
		st, err := os.Lstat(next)
		if err != nil || !protected(st) {
			return false
		}
		if st.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return false
			}
			target, err := os.Readlink(next)
			if err != nil {
				return false
			}
			if filepath.IsAbs(target) {
				current = "/"
			}
			pending = append(strings.Split(target, "/"), pending...)
		} else {
			if len(pending) > 0 && !st.IsDir() {
				return false
			}
			current = next
		}
	}
	return true
}
