package fsext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// symlink creates target -> dest, skipping the test when the platform
// cannot create symlinks (matches the guard in glob_symlink_test.go).
func symlink(t *testing.T, dest, link string) {
	t.Helper()
	if err := os.Symlink(dest, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

func TestResolveWriteTarget_NonSymlinkReturnsItself(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	got, err := ResolveWriteTarget(path)
	require.NoError(t, err)
	require.Equal(t, path, got)
}

func TestResolveWriteTarget_MissingNonSymlinkReturnsItself(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "never-existed")

	got, err := ResolveWriteTarget(path)
	require.NoError(t, err)
	require.Equal(t, path, got)
}

func TestResolveWriteTarget_ResolvesExistingSymlinkToTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("content"), 0o644))
	link := filepath.Join(dir, "link.txt")
	symlink(t, real, link)

	got, err := ResolveWriteTarget(link)
	require.NoError(t, err)
	require.Equal(t, real, got)
}

func TestResolveWriteTarget_ResolvesDanglingSymlinkToMissingTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "logs", "current.log")
	link := filepath.Join(dir, "latest.log")
	symlink(t, missing, link)

	got, err := ResolveWriteTarget(link)
	require.NoError(t, err)
	require.Equal(t, missing, got)
	require.NoFileExists(t, missing, "resolving must not create anything")
}

func TestResolveWriteTarget_FollowsSymlinkChain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("content"), 0o644))
	mid := filepath.Join(dir, "mid.txt")
	symlink(t, real, mid)
	outer := filepath.Join(dir, "outer.txt")
	symlink(t, mid, outer)

	got, err := ResolveWriteTarget(outer)
	require.NoError(t, err)
	require.Equal(t, real, got)
}

func TestResolveWriteTarget_ReportsCycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	symlink(t, b, a)
	symlink(t, a, b)

	_, err := ResolveWriteTarget(a)
	require.Error(t, err)
}

// TestAtomicWriteFileIfUnchanged_ThroughResolvedTarget_LeavesLinkIntact
// pins the fix for editing a symlinked file: writing to the file
// ResolveWriteTarget reports lands on the real target, not the link, so
// the link's own type and destination survive the write untouched.
func TestAtomicWriteFileIfUnchanged_ThroughResolvedTarget_LeavesLinkIntact(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("old"), 0o644))
	link := filepath.Join(dir, "link.txt")
	symlink(t, real, link)

	target, err := ResolveWriteTarget(link)
	require.NoError(t, err)
	require.NoError(t, AtomicWriteFileIfUnchanged(target, []byte("old"), []byte("new"), 0o644, true))

	content, err := os.ReadFile(real)
	require.NoError(t, err)
	require.Equal(t, "new", string(content))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.True(t, info.Mode()&os.ModeSymlink != 0, "the link must still be a symlink, not a regular file")
	dest, err := os.Readlink(link)
	require.NoError(t, err)
	require.Equal(t, real, dest, "the link must still point at its original target")
}

// TestAtomicWriteFileIfUnchanged_ThroughResolvedDanglingTarget_CreatesFile
// pins the fix for writing through a dangling symlink: writing to the
// file ResolveWriteTarget reports for a dangling link creates that file
// (AtomicCreateFile's create-only-if-absent path, not a compare-and-swap
// against something that was never there), which makes the link resolve.
func TestAtomicWriteFileIfUnchanged_ThroughResolvedDanglingTarget_CreatesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "logs", "current.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(real), 0o755))
	link := filepath.Join(dir, "latest.log")
	symlink(t, real, link)

	target, err := ResolveWriteTarget(link)
	require.NoError(t, err)
	require.NoError(t, AtomicWriteFileIfUnchanged(target, nil, []byte("first run"), 0o644, false))

	content, err := os.ReadFile(link)
	require.NoError(t, err)
	require.Equal(t, "first run", string(content), "the now-valid link must resolve to the created content")

	info, err := os.Lstat(link)
	require.NoError(t, err)
	require.True(t, info.Mode()&os.ModeSymlink != 0, "the link itself must be untouched")
}
