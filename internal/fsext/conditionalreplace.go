package fsext

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/lock"
)

type pathLock struct {
	mutex sync.Mutex
	users int
}

var conditionalReplaceState = struct {
	sync.Mutex
	locks       map[string]*pathLock
	beforeCheck func(string)
	readFile    func(string) ([]byte, error)
	rename      func(string, string) error
}{
	locks:       make(map[string]*pathLock),
	beforeCheck: func(string) {},
	readFile:    os.ReadFile,
	rename:      os.Rename,
}

func lockMutationPath(path string) func() {
	conditionalReplaceState.Lock()
	lock := conditionalReplaceState.locks[path]
	if lock == nil {
		lock = &pathLock{}
		conditionalReplaceState.locks[path] = lock
	}
	lock.users++
	conditionalReplaceState.Unlock()

	lock.mutex.Lock()
	return func() {
		lock.mutex.Unlock()
		conditionalReplaceState.Lock()
		lock.users--
		if lock.users == 0 {
			delete(conditionalReplaceState.locks, path)
		}
		conditionalReplaceState.Unlock()
	}
}

// conditionalReplaceLockDeadline bounds how long conditionalReplaceExisting
// waits for the cross-process lock before giving up. A few seconds is
// plenty for honest contention; longer suggests something is wedged.
const conditionalReplaceLockDeadline = 5 * time.Second

// conditionalReplaceLockDir holds the lock files that serialize
// conditionalReplaceExisting against other processes, e.g. two sennit
// instances editing the same file concurrently. It lives outside the
// target file's own directory (unlike config's per-file ".lock", which
// sits next to a single well-known path) because conditionalReplaceExisting
// runs against arbitrary paths across a user's project tree, and a stray
// "*.go.lock" next to every edited file would clutter `git status`.
var conditionalReplaceLockDir = filepath.Join(os.TempDir(), "sennit-fsext-locks")

// conditionalReplaceLockPath maps path to a lock file under
// conditionalReplaceLockDir, keyed by content hash so unrelated paths never
// collide and the lock file name never leaks the original path length or
// characters into a shared temp directory.
func conditionalReplaceLockPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(conditionalReplaceLockDir, hex.EncodeToString(sum[:])+".lock")
}

// conditionalReplaceExisting replaces path's contents with data if and only
// if path's current on-disk content still equals expected, returning
// ErrFileChanged otherwise. This is the "existing file" half of
// [AtomicWriteFileIfUnchanged]'s compare-and-swap.
//
// The read-compare-rename sequence is protected two ways: lockMutationPath
// serializes callers within this process, and a flock (see
// conditionalReplaceLockPath) serializes across processes — closing the
// TOCTOU window where another process writes path between the compare and
// the rename. This function's caller, the file-edit tool
// (internal/agent/tools/filemutation.go), does not otherwise hold any
// lock on path, unlike internal/config's store, which already wraps its
// own writes in a flock (see configFile.atomicWrite) and does not go
// through this function at all — it calls AtomicWriteFile directly.
func conditionalReplaceExisting(path string, expected, data []byte, mode os.FileMode) error {
	path = filepath.Clean(path)
	unlock := lockMutationPath(path)
	defer unlock()

	if err := os.MkdirAll(conditionalReplaceLockDir, 0o700); err != nil {
		return fmt.Errorf("create cross-process lock directory: %w", err)
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), conditionalReplaceLockDeadline)
	defer cancel()
	releaseFlock, err := lock.File(lockCtx, conditionalReplaceLockPath(path))
	if err != nil {
		return fmt.Errorf("acquire cross-process lock: %w", err)
	}
	defer releaseFlock()

	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	defer cleanup()

	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	conditionalReplaceState.Lock()
	beforeCheck := conditionalReplaceState.beforeCheck
	readFile := conditionalReplaceState.readFile
	rename := conditionalReplaceState.rename
	conditionalReplaceState.Unlock()
	beforeCheck(path)

	current, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrFileChanged
	}
	if err != nil {
		return fmt.Errorf("read destination before replacement: %w", err)
	}
	if !bytes.Equal(current, expected) {
		return ErrFileChanged
	}
	if err := rename(tempPath, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}
