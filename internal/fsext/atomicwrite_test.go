package fsext

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAtomicWriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	require.NoError(t, AtomicWriteFile(path, []byte(`{"key":"value"}`), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"key":"value"}`, string(data))

	// No temp files should linger.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "test.json", entries[0].Name())
}

func TestAtomicWriteFile_SyncedContentAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix file permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "write.json")

	require.NoError(t, AtomicWriteFile(path, []byte(`{"a":1}`), 0o640))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"a":1}`, string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

func TestAtomicCreateFile_SyncedContentAndMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix file permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "create.json")

	require.NoError(t, AtomicCreateFile(path, []byte(`{"b":2}`), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"b":2}`, string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestAtomicCreateFile_FallsBackWhenLinkUnsupported pins the behavior added
// for filesystems that cannot hard-link at all (some network mounts,
// overlay setups, FAT/exFAT): AtomicCreateFile must fall back to an
// exclusive O_CREATE|O_EXCL create instead of surfacing os.Link's error,
// and the file must land with the requested content and mode.
func TestAtomicCreateFile_FallsBackWhenLinkUnsupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix file permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "create.json")

	original := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.ENOSYS} }
	t.Cleanup(func() { linkFile = original })

	require.NoError(t, AtomicCreateFile(path, []byte(`{"b":2}`), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"b":2}`, string(data))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// No temp files should linger.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "create.json", entries[0].Name())
}

// TestAtomicCreateFile_ExistErrorNotTreatedAsUnsupported pins the
// distinction ErrFileChanged depends on: when os.Link fails because path
// already exists, that must still surface as os.ErrExist rather than
// being swallowed by the isUnsupportedLinkError fallback.
func TestAtomicCreateFile_ExistErrorNotTreatedAsUnsupported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "create.json")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o600))

	original := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EEXIST} }
	t.Cleanup(func() { linkFile = original })

	err := AtomicCreateFile(path, []byte(`{"b":2}`), 0o600)
	require.ErrorIs(t, err, os.ErrExist)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original", string(data), "the fallback must not have run and overwritten the existing file")
}

// TestAtomicCreateFile_FallbackStillExclusive pins that the O_EXCL
// fallback keeps its create-only-if-absent guarantee even though it is no
// longer atomic against the temp file: a concurrent creator that wins the
// race must not be overwritten.
func TestAtomicCreateFile_FallbackStillExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "create.json")

	original := linkFile
	linkFile = func(string, string) error {
		// Simulate another writer landing the file between our link
		// attempt and the fallback's own O_EXCL create.
		require.NoError(t, os.WriteFile(path, []byte("winner"), 0o600))
		return &os.LinkError{Op: "link", Err: syscall.ENOSYS}
	}
	t.Cleanup(func() { linkFile = original })

	err := AtomicCreateFile(path, []byte("loser"), 0o600)
	require.Error(t, err)
	require.True(t, errors.Is(err, os.ErrExist), "O_EXCL must report the race as an exist error")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "winner", string(data))
}

func TestAtomicWriteFile_PermissionsApplied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support Unix file permissions")
	}
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	require.NoError(t, AtomicWriteFile(path, []byte(`{}`), 0o600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
