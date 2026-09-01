package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lock"
)

const configLockDeadline = 5 * time.Second

var errAtomicWriteNoop = errors.New("config mutation is a no-op")

func configWriteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), configLockDeadline)
}

// configFile owns the in-process serialization of config file writes. Its
// mu is the only state it holds: everything else a write needs (which path,
// what bytes) is passed in by the caller, so configFile knows nothing about
// Config, providers, or any other ConfigStore concern — just "one write to
// this path at a time, paired with a cross-process flock."
//
// mu nests inside writeMu whenever a mutator persists as part of a
// copy-on-write publish (see updateLocked), but is also taken standalone by
// callers (SetConfigFields, RemoveConfigField) that persist before
// reloading. It never leads to acquiring writeMu itself, so that is the
// only order that matters: writeMu, if held, is always the outer lock.
type configFile struct {
	mu sync.Mutex
}

// lock acquires both the in-process mutex and a cross-process flock on the
// config file at path. Callers that need to do I/O between reading and
// writing (e.g. an HTTP token exchange) must use lock explicitly rather
// than atomicWrite.
//
// The returned release function drops both locks. Callers must call it as
// soon as the file access is complete — no I/O should be performed while
// the lock is held.
type tryLocker interface {
	TryLock() bool
	Lock()
	Unlock()
}

func lockMutexContext(ctx context.Context, mu tryLocker) error {
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

func (f *configFile) lock(ctx context.Context, path string) (func(), error) {
	if err := lockMutexContext(ctx, &f.mu); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		f.mu.Unlock()
		return nil, fmt.Errorf("acquire config lock: %w", err)
	}
	return func() {
		release()
		f.mu.Unlock()
	}, nil
}

// atomicWrite handles the lock-read-transform-write-unlock cycle for a
// config file mutation at path. The fn callback receives the current file
// contents (raw bytes, or {} if the file is missing) and must return the
// new contents. fn must be pure — no I/O, no network calls.
func (f *configFile) atomicWrite(ctx context.Context, path string, fn func(current []byte) ([]byte, error)) error {
	unlock, err := f.lock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("{}")
		} else {
			return fmt.Errorf("read config file: %w", err)
		}
	}

	newData, err := fn(data)
	if errors.Is(err, errAtomicWriteNoop) {
		return nil
	}
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	return fsext.AtomicWriteFile(path, newData, 0o600)
}
