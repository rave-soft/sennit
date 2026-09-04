package workspacelock

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

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/lock"
	"github.com/rave-soft/sennit/internal/version"
)

// ErrLocked is returned by Acquire when a project's workspace directory
// is already in use by another sennit process.
var ErrLocked = errors.New("workspace already in use by another sennit process")

// lockFileName is the name of the lock file inside a project's .sennit
// directory. It lives next to sennit.db so users can `ls` and find it.
const lockFileName = brand.LockFile

// ownerInfo is the JSON payload written into the lock file by
// the process that currently owns it. It is purely informational; the
// authoritative state of ownership is the operating system flock on
// the file descriptor.
type ownerInfo struct {
	PID       int    `json:"pid"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
}

// Lock represents an acquired exclusive lock on a project's workspace
// directory (its .sennit directory). Calling Release on a nil *Lock is a
// no-op, so callers that skip locking can hold a nil lock and release it
// unconditionally.
type Lock struct {
	dir  string
	once sync.Once
	// enforced records whether this lock actually excludes another
	// process. It is false when SENNIT_SKIP_DATADIR_LOCK made Acquire hand
	// back a no-op lock, which callers doing something that is only safe
	// under mutual exclusion have to know about - see Enforced.
	enforced bool
}

// Enforced reports whether holding this lock actually keeps a second
// sennit out of the same workspace. It is false only when
// SENNIT_SKIP_DATADIR_LOCK turned acquisition into a no-op, and work that
// is safe solely because "no other process is running turns against these
// sessions" must ask before doing it. Finalizing interrupted turns is
// exactly that work: it writes tool-result errors and a canceled finish
// into every unfinished assistant message it finds, which is repair for a
// crashed run and corruption for a live one.
func (l *Lock) Enforced() bool {
	return l != nil && l.enforced
}

// poolEntry is the process-local, refcounted OS lock backing every
// in-process Lock for a given directory. Refcounting mirrors the DB
// connection pool in internal/db's connect.go: the same process may
// legitimately want the same workspace directory locked from more than
// one place at once (e.g. two concurrent CreateWorkspace calls for the
// same path racing each other before either registers), and only the
// first acquirer should take the actual OS-level flock; the rest just
// bump the refcount. The OS lock is released once the last in-process
// holder releases.
type poolEntry struct {
	release  func()
	refCount int
	// enforced mirrors Lock.enforced for every later acquirer that joins
	// this entry by refcount rather than taking the OS lock itself.
	enforced bool
}

var (
	pool   = make(map[string]*poolEntry)
	poolMu sync.Mutex
)

// Release drops the lock. Safe to call on a nil *Lock.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		poolMu.Lock()
		defer poolMu.Unlock()

		entry, ok := pool[l.dir]
		if !ok {
			return
		}
		entry.refCount--
		if entry.refCount > 0 {
			return
		}
		delete(pool, l.dir)
		entry.release()
	})
}

// Acquire takes an exclusive non-blocking lock on {dir}/sennit.lock,
// guarding against two sennit processes racing the same project's
// workspace. If the lock is already held by another process, it returns
// ErrLocked wrapped with a diagnostic that includes whatever owner info
// that process wrote. Concurrent acquisitions for the same directory
// within this process share one underlying OS lock via refcounting; see
// [poolEntry].
//
// Acquisition is skipped (returning a no-op lock) when
// SENNIT_SKIP_DATADIR_LOCK is set to a truthy value. This is intended
// as an escape hatch for hostile filesystems that do not implement
// advisory locking; it should not be used in normal operation.
func Acquire(dir string) (*Lock, error) {
	absDir, err := canonicalDir(dir)
	if err != nil {
		return nil, err
	}

	poolMu.Lock()
	defer poolMu.Unlock()

	if entry, ok := pool[absDir]; ok {
		entry.refCount++
		return &Lock{dir: absDir, enforced: entry.enforced}, nil
	}

	if skipLock() {
		pool[absDir] = &poolEntry{release: func() {}, refCount: 1}
		return &Lock{dir: absDir}, nil
	}

	path := filepath.Join(absDir, lockFileName)
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
	pool[absDir] = &poolEntry{release: release, refCount: 1, enforced: true}
	return &Lock{dir: absDir, enforced: true}, nil
}

// canonicalDir returns an absolute, symlink-canonical directory
// identity so aliases share one in-process lock entry.
func canonicalDir(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("failed to make workspace lock directory absolute %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(absDir), nil
		}
		return "", fmt.Errorf("failed to canonicalize workspace lock directory %q: %w", dir, err)
	}
	return filepath.Clean(resolved), nil
}

func skipLock() bool {
	v, _ := strconv.ParseBool(os.Getenv(brand.EnvPrefix + "SKIP_DATADIR_LOCK"))
	return v
}

// writeOwnerInfo truncates and rewrites the lock file with the current
// process's identifying information. It is called only after the lock
// is held.
func writeOwnerInfo(path string) error {
	info := ownerInfo{
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
func readOwnerInfo(path string) ownerInfo {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return ownerInfo{}
	}
	var info ownerInfo
	_ = json.Unmarshal(raw, &info)
	return info
}

// contendedLockError builds a wrapped ErrLocked annotated with whatever
// owner metadata is currently in the lock file.
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
	return fmt.Errorf("%w: %s%s", ErrLocked, dir, details)
}
