//go:build windows

package git

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// blockRemoval provokes a worktree-removal failure the way it actually
// happens on Windows.
//
// The POSIX mechanism (strip write permission from wtPath's parent, so
// unlinking wtPath's own directory entry fails) has no equivalent here:
// os.Chmod on Windows only ever toggles the read-only attribute, which
// does not gate directory-entry removal the way a POSIX write-permission
// bit does — it would not reproduce the failure at all. Nor does simply
// holding a Go *os.File open inside the worktree: Go always opens files
// on Windows with FILE_SHARE_DELETE set, specifically so os.Remove keeps
// working on an open file the way POSIX unlink does, so an ordinary held
// handle would not block anything either.
//
// What genuinely blocks a Windows delete is a handle opened without
// FILE_SHARE_DELETE — "someone still has this open" (ERROR_SHARING_
// VIOLATION), which needs CreateFile directly. That is what this does:
// write a file inside the worktree (deep in what WorktreeRemove's
// makeDirsWritable would otherwise make fully removable) and open it
// exclusively of delete sharing.
func blockRemoval(t *testing.T, parent, wtPath string) func() {
	t.Helper()

	target := filepath.Join(wtPath, "locked.txt")
	require.NoError(t, os.WriteFile(target, []byte("locked"), 0o644))

	p, err := windows.UTF16PtrFromString(target)
	require.NoError(t, err)
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ, // deliberately no FILE_SHARE_DELETE
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err)

	var once sync.Once
	return func() {
		once.Do(func() { _ = windows.CloseHandle(h) })
	}
}
