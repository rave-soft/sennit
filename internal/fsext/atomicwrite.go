package fsext

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// AtomicWriteFile writes data to a file atomically by writing to a unique
// temporary file in the same directory and renaming it into place. This
// prevents concurrent readers from observing a partially-written file.
var ErrFileChanged = errors.New("file changed")

// linkFile is os.Link, indirected so tests can simulate a filesystem that
// cannot hard-link (see isUnsupportedLinkError) without needing an actual
// one.
var linkFile = os.Link

func AtomicWriteFileIfUnchanged(path string, expected, data []byte, mode os.FileMode, exists bool) error {
	if exists {
		return conditionalReplaceExisting(filepath.Clean(path), expected, data, mode)
	}
	if err := AtomicCreateFile(path, data, mode); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrFileChanged
		}
		return err
	}
	return nil
}

func AtomicCreateFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = f.Close(); _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := linkFile(tmp, path); err != nil {
		// os.ErrExist means path is already there — callers rely on
		// that distinction (see AtomicWriteFileIfUnchanged's ErrFileChanged
		// mapping) so it must pass through unchanged. Anything else that
		// isUnsupportedLinkError recognizes means this filesystem cannot
		// hard-link at all, so fall back to an exclusive create.
		if !errors.Is(err, os.ErrExist) && isUnsupportedLinkError(err) {
			fallbackErr := createFileExclusive(path, data, perm)
			_ = os.Remove(tmp)
			return fallbackErr
		}
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(tmp)
	syncDir(dir)
	return nil
}

// createFileExclusive is AtomicCreateFile's fallback for filesystems that
// cannot hard-link (see isUnsupportedLinkError). O_EXCL still refuses to
// create over an existing file, so the create-only-if-absent guarantee
// holds, but the write is no longer atomic with respect to the already
// fully-written temp file: a crash partway through this function can
// leave path partially written, which the hard-link path never allows.
func createFileExclusive(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the write error above is what matters
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the chmod error above is what matters
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the sync error above is what matters
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the close error above is what matters
		return err
	}
	if err := renameFile(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the rename error above is what matters
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry so a completed rename survives a
// crash. Best-effort: some filesystems and platforms (notably Windows) do
// not permit opening a directory for sync, and a failure here does not
// invalidate the write that already landed.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// renameRetryBudget bounds how long renameFile keeps retrying transient
// failures before giving up and returning the error.
const renameRetryBudget = 2 * time.Second

// renameFile renames tmp over path. On Windows the rename fails with
// ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION while another process
// (antivirus, search indexer) or a concurrent reader briefly holds a
// handle on the destination, so transient failures are retried with
// backoff. On other platforms isTransientRenameError is always false
// and this is a plain os.Rename.
func renameFile(tmp, path string) error {
	var slept time.Duration
	delay := time.Millisecond
	for {
		err := os.Rename(tmp, path)
		if err == nil || !isTransientRenameError(err) || slept >= renameRetryBudget {
			return err
		}
		time.Sleep(delay)
		slept += delay
		delay = min(delay*2, 50*time.Millisecond)
	}
}
