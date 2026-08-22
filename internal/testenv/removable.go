package testenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// AssertRemovableOnWindows fails t if dir still contains an open file
// handle or is the calling process's current working directory at the
// moment it is called. Register it immediately after t.TempDir() so
// t.Cleanup's LIFO order runs it just before TempDir's own RemoveAll
// cleanup: what it reports here is exactly what Windows would refuse
// to delete, checked on a platform (Linux) where the deletion itself
// silently succeeds regardless.
//
// The open-handle check walks /proc/self/fd, which is Linux-only; on
// any other OS this is a no-op, so it never fails a macOS run.
func AssertRemovableOnWindows(t testing.TB, dir string) {
	t.Helper()

	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Errorf("testenv: resolve %q: %v", dir, err)
		return
	}
	absDir = filepath.Clean(absDir)

	if cwd, err := os.Getwd(); err != nil {
		t.Errorf("testenv: os.Getwd: %v", err)
	} else if within(filepath.Clean(cwd), absDir) {
		t.Errorf("testenv: process cwd %q is still inside %q; Windows would refuse to remove it (missing os.Chdir restore in a test cleanup)", cwd, absDir)
	}

	if runtime.GOOS != "linux" {
		return
	}

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		// Not fatal: some sandboxes hide /proc/self/fd. Nothing more to check.
		return
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			// The fd can vanish between ReadDir and Readlink; not our concern.
			continue
		}
		if within(filepath.Clean(target), absDir) {
			t.Errorf("testenv: fd %s is still open on %q (inside %q); Windows would refuse to remove it", entry.Name(), target, absDir)
		}
	}
}

// RestoreCwd captures the process's current working directory and
// registers a t.Cleanup that restores it, so a command under test that
// os.Chdir's on its own (as Sennit's --cwd flag resolution does; see
// internal/cmd/root.go's ResolveCwd) does not leave the test process's
// cwd sitting inside a directory a later t.Cleanup wants to remove.
//
// Call it after any directory the command being invoked might chdir
// into already exists (typically right where the "cwd" flag is set),
// so t.Cleanup's LIFO order runs this restore before that directory's
// own t.TempDir() removal.
func RestoreCwd(t testing.TB) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("testenv: os.Getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Errorf("testenv: restore cwd to %q: %v", orig, err)
		}
	})
}

// within reports whether path equals dir or is nested under it, using a
// separator-bounded prefix check so /tmp/foo does not match /tmp/foobar.
func within(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(os.PathSeparator))
}
