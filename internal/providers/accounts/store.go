package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/lock"
)

// Store persists Accounts, grouped by provider. Implementations must be
// safe for concurrent use, including from other processes.
type Store interface {
	// List returns providerID's accounts in stable, on-disk order. The
	// returned slice is a copy; mutating it does not affect the store.
	List(providerID string) ([]Account, error)
	// Get returns one account by ID. A missing account is reported as
	// (Account{}, false, nil), not an error.
	Get(providerID, accountID string) (Account, bool, error)
	// Upsert validates a and inserts it, or replaces the existing
	// account with the same ID in place (same position, no duplicate).
	Upsert(providerID string, a Account) error
	// Remove deletes an account by ID. Removing an account that does
	// not exist is not an error.
	Remove(providerID, accountID string) error
	// RecordUsage updates an existing account's usage snapshot. It is
	// an error to record usage for an account that does not exist.
	RecordUsage(providerID, accountID string, u Usage) error
}

// storeVersion is the current on-disk schema version. Bump it and add
// migration logic if the file's shape ever changes incompatibly.
const storeVersion = 1

// lockTimeout bounds how long a mutation waits to acquire the
// cross-process file lock before giving up. 10s is generous for a plain
// read-modify-write of a small JSON file — long enough to ride out a
// concurrent writer without making a caller hang if something is
// genuinely stuck (e.g. a crashed process left the lock file open, which
// the kernel already frees, or a much larger-than-expected file).
const lockTimeout = 10 * time.Second

// FileStore is a Store backed by a single JSON file on disk, guarded by a
// cross-process advisory lock (internal/lock) for every mutation.
//
// FileStore holds no in-memory cache of the file's contents: another
// process may have changed it since the last call, and correctness here
// matters more than saving a read. Every method re-reads the file itself.
type FileStore struct {
	path string
}

// NewFileStore returns a FileStore backed by the file at path. The caller
// (not this package) decides where that file lives, to keep accounts free
// of a dependency on internal/config.
func NewFileStore(path string) *FileStore {
	return &FileStore{path: path}
}

// fileFormat is the on-disk shape of the account file.
type fileFormat struct {
	Version  int                  `json:"version"`
	Accounts map[string][]Account `json:"accounts"`
}

// List implements Store.
func (s *FileStore) List(providerID string) ([]Account, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	accs := f.Accounts[providerID]
	out := make([]Account, len(accs))
	copy(out, accs)
	return out, nil
}

// Get implements Store.
func (s *FileStore) Get(providerID, accountID string) (Account, bool, error) {
	f, err := s.read()
	if err != nil {
		return Account{}, false, err
	}
	for _, a := range f.Accounts[providerID] {
		if a.ID == accountID {
			return a, true, nil
		}
	}
	return Account{}, false, nil
}

// Upsert implements Store.
func (s *FileStore) Upsert(providerID string, a Account) error {
	if err := a.Validate(); err != nil {
		return fmt.Errorf("invalid account: %w", err)
	}
	return s.mutate(func(f *fileFormat) error {
		accs := f.Accounts[providerID]
		for i := range accs {
			if accs[i].ID == a.ID {
				accs[i] = a
				return nil
			}
		}
		f.Accounts[providerID] = append(accs, a)
		return nil
	})
}

// Remove implements Store. Removing an account that does not exist is a
// no-op, not an error, so callers don't have to Get before Remove just to
// stay idempotent.
func (s *FileStore) Remove(providerID, accountID string) error {
	return s.mutate(func(f *fileFormat) error {
		accs := f.Accounts[providerID]
		for i := range accs {
			if accs[i].ID == accountID {
				f.Accounts[providerID] = append(accs[:i], accs[i+1:]...)
				return nil
			}
		}
		return nil
	})
}

// RecordUsage implements Store.
func (s *FileStore) RecordUsage(providerID, accountID string, u Usage) error {
	return s.mutate(func(f *fileFormat) error {
		accs := f.Accounts[providerID]
		for i := range accs {
			if accs[i].ID == accountID {
				accs[i].Usage = u
				return nil
			}
		}
		return fmt.Errorf("account %q not found for provider %q", accountID, providerID)
	})
}

// read loads the file as it currently stands, treating a missing file as
// an empty store.
func (s *FileStore) read() (fileFormat, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileFormat{Version: storeVersion, Accounts: map[string][]Account{}}, nil
		}
		return fileFormat{}, fmt.Errorf("read account file %q: %w", s.path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return fileFormat{}, fmt.Errorf("parse account file %q: %w", s.path, err)
	}
	if f.Version > storeVersion {
		return fileFormat{}, fmt.Errorf(
			"account file %q has version %d, newer than the %d this build understands",
			s.path, f.Version, storeVersion)
	}
	if f.Accounts == nil {
		f.Accounts = map[string][]Account{}
	}
	return f, nil
}

// mutate performs one read-modify-write cycle under the cross-process
// file lock: read the current file, let fn change it in place, write the
// result back atomically. Every Store mutation goes through this so two
// Sennit processes touching the same file never clobber each other's
// accounts.
func (s *FileStore) mutate(fn func(f *fileFormat) error) error {
	// The account file typically lives in a global data directory that
	// may not exist yet on a clean machine (first run, first account for
	// this provider). 0o700, not 0o755: the directory holds tokens.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create account directory for %q: %w", s.path, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()

	release, err := lock.File(ctx, s.path+".lock")
	if err != nil {
		return fmt.Errorf("lock account file %q: %w", s.path, err)
	}
	defer release()

	f, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&f); err != nil {
		return err
	}
	f.Version = storeVersion

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode account file %q: %w", s.path, err)
	}
	if err := fsext.AtomicWriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("write account file %q: %w", s.path, err)
	}
	return nil
}
