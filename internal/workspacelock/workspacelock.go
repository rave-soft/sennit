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

// Mode records what kind of process holds a workspace lock: an
// interactive TUI (or any other in-process, embedded caller) or a
// headless daemon serving the workspace over its unix socket.
type Mode string

const (
	// ModeTUI is the default: an in-process caller with no socket of its
	// own. It is also what an OwnerInfo read back from a lock file
	// written before Mode existed normalizes to — see CurrentOwner.
	ModeTUI Mode = "tui"
	// ModeDaemon is a `sennit daemon run` process; Socket names the unix
	// socket it listens on.
	ModeDaemon Mode = "daemon"
)

// OwnerInfo is the JSON payload written into the lock file by
// the process that currently owns it. It is purely informational; the
// authoritative state of ownership is the operating system flock on
// the file descriptor.
//
// Mode and Socket were added after this record first shipped. A lock
// file written by an older binary has neither field, which decodes as
// the zero Mode (""); CurrentOwner normalizes that to ModeTUI rather
// than leaving callers to special-case the empty string themselves.
type OwnerInfo struct {
	PID       int    `json:"pid"`
	Version   string `json:"version,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	Mode      Mode   `json:"mode,omitempty"`
	Socket    string `json:"socket,omitempty"`
}

// effectiveMode normalizes a possibly-old-format Mode: missing (from a
// pre-Mode lock file, or a zero OwnerInfo) reads as ModeTUI.
func (o OwnerInfo) effectiveMode() Mode {
	if o.Mode == "" {
		return ModeTUI
	}
	return o.Mode
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

// SetMode rewrites this lock's owner info with mode and socket, once the
// work that mode describes has actually happened -- a daemon calls this
// only after its unix socket is bound, not before, so a reader (a client
// deciding whether to dial, a future `sennit daemon status`) never sees a
// socket recorded that isn't live yet. It preserves the PID/Version/
// StartedAt this lock was acquired with; only Mode and Socket change.
//
// A no-op (nil error) on a nil *Lock or one that isn't Enforced: neither
// holds a real lock file to rewrite, and a caller that skipped locking
// (SENNIT_SKIP_DATADIR_LOCK) has nothing here to report to anyone else
// anyway.
func (l *Lock) SetMode(mode Mode, socket string) error {
	if l == nil || !l.enforced {
		return nil
	}
	path := filepath.Join(l.dir, lockFileName)
	info := readOwnerInfo(path)
	info.Mode = mode
	info.Socket = socket
	return writeOwnerInfoStruct(path, info)
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
//
// By default the owner info records ModeTUI; pass WithMode to record a
// daemon's mode and socket path instead.
func Acquire(dir string, opts ...AcquireOption) (*Lock, error) {
	cfg := acquireConfig{mode: ModeTUI}
	for _, opt := range opts {
		opt(&cfg)
	}

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
	if err := writeOwnerInfo(path, cfg.mode, cfg.socket); err != nil {
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

// acquireConfig carries Acquire's options.
type acquireConfig struct {
	mode   Mode
	socket string
}

// AcquireOption configures Acquire.
type AcquireOption func(*acquireConfig)

// WithMode records mode and (for ModeDaemon) socket in the lock file's
// owner info, in place of the default ModeTUI with no socket.
func WithMode(mode Mode, socket string) AcquireOption {
	return func(c *acquireConfig) {
		c.mode = mode
		c.socket = socket
	}
}

// writeOwnerInfo truncates and rewrites the lock file with the current
// process's identifying information. It is called only after the lock
// is held.
func writeOwnerInfo(path string, mode Mode, socket string) error {
	return writeOwnerInfoStruct(path, OwnerInfo{
		PID:       os.Getpid(),
		Version:   version.Version,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:      mode,
		Socket:    socket,
	})
}

// writeOwnerInfoStruct truncates and rewrites the lock file with info
// verbatim. Shared by writeOwnerInfo (a fresh acquisition) and
// Lock.SetMode (an update to one already held).
func writeOwnerInfoStruct(path string, info OwnerInfo) error {
	payload, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return os.WriteFile(path, payload, 0o600)
}

// readOwnerInfo returns the lock file's recorded owner, if it parses.
// A missing or malformed file yields an empty struct and no error;
// the caller decides what to surface to the user. Mode is left exactly
// as decoded (possibly "", for a pre-Mode lock file); callers that need
// the normalized value use effectiveMode or CurrentOwner.
func readOwnerInfo(path string) OwnerInfo {
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return OwnerInfo{}
	}
	var info OwnerInfo
	_ = json.Unmarshal(raw, &info)
	return info
}

// CurrentOwner reads the lock file's recorded owner for dir without
// taking the lock itself — for a caller (a client dialing a project's
// daemon, say) that needs to know who holds a workspace before deciding
// whether to contend for it. ok is false when dir has never been locked,
// or its lock file is missing or unreadable; Mode is normalized (a
// pre-Mode record reads as ModeTUI).
func CurrentOwner(dir string) (info OwnerInfo, ok bool, err error) {
	absDir, err := canonicalDir(dir)
	if err != nil {
		return OwnerInfo{}, false, err
	}
	path := filepath.Join(absDir, lockFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return OwnerInfo{}, false, nil
		}
		return OwnerInfo{}, false, fmt.Errorf("failed to read workspace lock owner info %q: %w", path, err)
	}
	if len(raw) == 0 {
		return OwnerInfo{}, false, nil
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return OwnerInfo{}, false, fmt.Errorf("failed to parse workspace lock owner info %q: %w", path, err)
	}
	info.Mode = info.effectiveMode()
	return info, true, nil
}

// contendedLockError builds a wrapped ErrLocked annotated with whatever
// owner metadata is currently in the lock file.
func contendedLockError(dir, lockPath string) error {
	info := readOwnerInfo(lockPath)
	details := ""
	switch {
	case info.PID != 0 && info.StartedAt != "":
		details = fmt.Sprintf(" (owner pid=%d version=%s started_at=%s mode=%s)",
			info.PID, info.Version, info.StartedAt, info.effectiveMode())
	case info.PID != 0:
		details = fmt.Sprintf(" (owner pid=%d mode=%s)", info.PID, info.effectiveMode())
	}
	return fmt.Errorf("%w: %s%s", ErrLocked, dir, details)
}
