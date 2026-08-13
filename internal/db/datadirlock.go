package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/rave-soft/braid/internal/lock"
	"github.com/rave-soft/braid/internal/version"
)

// ErrWorkspaceLocked is returned by AcquireWorkspaceLock when a
// project's workspace directory is already in use by another braid
// process.
var ErrWorkspaceLocked = errors.New("workspace already in use by another braid process")

// workspaceLockFile is the name of the lock file inside a project's
// .braid directory. It lives next to braid.db so users can `ls` and
// find it.
const workspaceLockFile = "braid.lock"

// dataDirOwnerInfo is the JSON payload written into the lock file by
// the process that currently owns it. It is purely informational; the
// authoritative state of ownership is the operating system flock on
// the file descriptor.
type dataDirOwnerInfo struct {
	PID       int    `json:"pid"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// WorkspaceLock represents an acquired exclusive lock on a project's
// workspace directory (its .braid directory). Calling Release on a nil
// *WorkspaceLock is a no-op, so callers that skip locking can hold a
// nil lock and release it unconditionally.
type WorkspaceLock struct {
	dir  string
	once sync.Once
}

// workspaceLockEntry is the process-local, refcounted OS lock backing
// every in-process WorkspaceLock for a given directory. Refcounting
// mirrors the DB connection pool in connect.go: the same process may
// legitimately want the same workspace directory locked from more than
// one place at once (e.g. two concurrent CreateWorkspace calls for the
// same path racing each other before either registers), and only the
// first acquirer should take the actual OS-level flock; the rest just
// bump the refcount. The OS lock is released once the last in-process
// holder releases.
type workspaceLockEntry struct {
	release  func()
	refCount int
}

var (
	workspaceLockPool   = make(map[string]*workspaceLockEntry)
	workspaceLockPoolMu sync.Mutex
)

// Release drops the lock. Safe to call on a nil *WorkspaceLock.
func (l *WorkspaceLock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		workspaceLockPoolMu.Lock()
		defer workspaceLockPoolMu.Unlock()

		entry, ok := workspaceLockPool[l.dir]
		if !ok {
			return
		}
		entry.refCount--
		if entry.refCount > 0 {
			return
		}
		delete(workspaceLockPool, l.dir)
		entry.release()
	})
}

// AcquireWorkspaceLock takes an exclusive non-blocking lock on
// {dir}/braid.lock, guarding against two braid processes racing the
// same project's workspace. If the lock is already held by another
// process, it returns ErrWorkspaceLocked wrapped with a diagnostic
// that includes whatever owner info that process wrote. Concurrent
// acquisitions for the same directory within this process share one
// underlying OS lock via refcounting; see [workspaceLockEntry].
//
// Acquisition is skipped (returning a no-op lock) when
// BRAID_SKIP_DATADIR_LOCK is set to a truthy value. This is intended
// as an escape hatch for hostile filesystems that do not implement
// advisory locking; it should not be used in normal operation.
func AcquireWorkspaceLock(dir string) (*WorkspaceLock, error) {
	absDir, err := canonicalWorkspaceLockDir(dir)
	if err != nil {
		return nil, err
	}

	workspaceLockPoolMu.Lock()
	defer workspaceLockPoolMu.Unlock()

	if entry, ok := workspaceLockPool[absDir]; ok {
		entry.refCount++
		return &WorkspaceLock{dir: absDir}, nil
	}

	if skipWorkspaceLock() {
		workspaceLockPool[absDir] = &workspaceLockEntry{release: func() {}, refCount: 1}
		return &WorkspaceLock{dir: absDir}, nil
	}

	path := filepath.Join(absDir, workspaceLockFile)
	release, err := lock.TryFile(path)
	if err != nil {
		if errors.Is(err, lock.ErrContended) {
			return nil, contendedLockError(dir, path)
		}
		return nil, fmt.Errorf("failed to lock workspace directory %q: %w", dir, err)
	}

	// Record ownership metadata so a contending process can identify
	// us. Failures here are non-fatal: the OS-level lock is what
	// actually guarantees mutual exclusion, and a missing/partial JSON
	// payload only degrades the diagnostic a contender prints.
	if err := writeOwnerInfo(path); err != nil {
		slog.Debug("Failed to write workspace lock owner info", "path", path, "error", err)
	}

	// The lock file itself is intentionally never unlinked. flock is
	// keyed by inode, not by path, and any close-then-unlink (or
	// unlink-then-close) ordering opens a window where two processes
	// can each hold a flock on a different inode that lives at the
	// same path. Leaving the file in place lets every acquirer see
	// the same inode and lets the kernel arbitrate correctly.
	workspaceLockPool[absDir] = &workspaceLockEntry{release: release, refCount: 1}
	return &WorkspaceLock{dir: absDir}, nil
}

// canonicalWorkspaceLockDir returns an absolute, symlink-canonical directory
// identity so aliases share one in-process lock entry.
func canonicalWorkspaceLockDir(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to make workspace lock directory absolute %q: %w", dir, err)
	}
	canonicalDir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(absDir), nil
		}
		return "", fmt.Errorf("failed to canonicalize workspace lock directory %q: %w", dir, err)
	}
	return filepath.Clean(canonicalDir), nil
}

func skipWorkspaceLock() bool {
	v, _ := strconv.ParseBool(os.Getenv("BRAID_SKIP_DATADIR_LOCK"))
	return v
}

// writeOwnerInfo truncates and rewrites the lock file with the current
// process's identifying information. It is called only after the lock
// is held.
func writeOwnerInfo(path string) error {
	info := dataDirOwnerInfo{
		PID:       os.Getpid(),
		Version:   version.Version,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(path, payload, 0o600)
}

// readOwnerInfo returns the lock file's recorded owner, if it parses.
// A missing or malformed file yields an empty struct and no error;
// the caller decides what to surface to the user.
func readOwnerInfo(path string) dataDirOwnerInfo {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return dataDirOwnerInfo{}
	}
	var info dataDirOwnerInfo
	_ = json.Unmarshal(raw, &info)
	return info
}

// contendedLockError builds a wrapped ErrWorkspaceLocked annotated with
// whatever owner metadata is currently in the lock file.
func contendedLockError(dir, lockPath string) error {
	info := readOwnerInfo(lockPath)
	details := ""
	switch {
	case info.PID != 0 && info.StartedAt != "":
		details = fmt.Sprintf(" (owner pid=%d version=%s started_at=%s)",
			info.PID, info.Version, info.StartedAt)
	case info.PID != 0:
		details = fmt.Sprintf(" (owner pid=%d)", info.PID)
	}
	return fmt.Errorf("%w: %s%s", ErrWorkspaceLocked, dir, details)
}
