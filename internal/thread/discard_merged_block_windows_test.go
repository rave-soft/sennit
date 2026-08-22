//go:build windows

package thread_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// blockWorktreeRemoval provokes a worktree-removal failure the way it
// actually happens on Windows. See internal/git's blockRemoval (the same
// mechanism, duplicated here because it is unexported test code in another
// package) for why the POSIX approach — stripping write permission from
// the worktree's parent — does not reproduce anything on Windows: os.Chmod
// there only toggles the read-only attribute, which does not gate
// directory-entry removal.
//
// What genuinely blocks a Windows delete is a handle opened without
// FILE_SHARE_DELETE — "someone still has this open" (ERROR_SHARING_
// VIOLATION), which needs CreateFile directly. This writes a file inside
// the worktree and opens it exclusively of delete sharing, so
// [git.WorktreeRemove]'s os.RemoveAll fails on it even after its own
// chmod pass has made every directory writable.
func blockWorktreeRemoval(t *testing.T, worktreePath string) func() {
	t.Helper()

	target := filepath.Join(worktreePath, "locked.txt")
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
