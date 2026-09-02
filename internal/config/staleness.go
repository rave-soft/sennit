package config

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// fileSnapshot captures metadata about a config file at a point in time.
type fileSnapshot struct {
	Path    string
	Exists  bool
	Size    int64
	ModTime int64 // UnixNano
}

// StalenessResult contains the result of a staleness check.
type StalenessResult struct {
	Dirty   bool
	Changed []string
	Missing []string
	Errors  map[string]error // stat errors by path
}

// fileStaleness owns the snapshots used to distinguish external file changes
// from writes performed by this process. Its mutex deliberately covers an
// atomic config-file write and its following refresh, so a poller cannot see
// the new on-disk stamp against the old snapshot.
type fileStaleness struct {
	mu      sync.Mutex
	tracked []string
	snaps   map[string]fileSnapshot
}

func (f *fileStaleness) check() StalenessResult {
	f.mu.Lock()
	defer f.mu.Unlock()

	var result StalenessResult
	result.Errors = make(map[string]error)
	for _, path := range f.tracked {
		snapshot, hadSnapshot := f.snaps[path]
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()
		if err != nil && !os.IsNotExist(err) {
			result.Errors[path] = err
			result.Dirty = true
		}
		if !exists {
			if hadSnapshot && snapshot.Exists {
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}
		if !hadSnapshot || !snapshot.Exists || snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}
	slices.Sort(result.Changed)
	slices.Sort(result.Missing)
	return result
}

func statSnapshot(path string) fileSnapshot {
	info, err := os.Stat(path)
	exists := err == nil && !info.IsDir()
	snapshot := fileSnapshot{Path: path, Exists: exists}
	if exists {
		snapshot.Size = info.Size()
		snapshot.ModTime = info.ModTime().UnixNano()
	}
	return snapshot
}

// refreshLocked captures all currently tracked paths. Caller must hold f.mu.
func (f *fileStaleness) refreshLocked(preRead map[string]fileSnapshot) {
	if f.snaps == nil {
		f.snaps = make(map[string]fileSnapshot)
	}
	for _, path := range f.tracked {
		if snapshot, ok := preRead[path]; ok {
			f.snaps[path] = snapshot
			continue
		}
		f.snaps[path] = statSnapshot(path)
	}
}

// addAndRefreshLocked ensures path is a member of the tracked set — without
// dropping any existing member, unlike capture — then refreshes every
// tracked path's snapshot. Caller must hold f.mu.
//
// Used by typed mutators (updateLocked): the scope's own path is normally
// already tracked (Load/reloadFromDisk seed the set from every candidate
// lookupConfigs returns, including global layers absent on disk), but this
// guards the case of a store whose tracked set was built some other way.
func (f *fileStaleness) addAndRefreshLocked(path string) {
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if !slices.Contains(f.tracked, abs) {
			f.tracked = append(f.tracked, abs)
			slices.Sort(f.tracked)
		}
	}
	f.refreshLocked(nil)
}

func (f *fileStaleness) capture(paths, requiredPaths []string, preRead map[string]fileSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[string]struct{})
	addPaths := func(paths []string) {
		for _, path := range paths {
			if path == "" {
				continue
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				abs = path
			}
			seen[abs] = struct{}{}
		}
	}
	addPaths(paths)
	addPaths(requiredPaths)
	f.tracked = make([]string, 0, len(seen))
	for path := range seen {
		f.tracked = append(f.tracked, path)
	}
	slices.Sort(f.tracked)
	f.refreshLocked(preRead)
}

// withWrite runs fn, which performs a config-file write, and — if fn reports
// wrote — refreshes every tracked path's snapshot before releasing the lock.
// Holding f.mu across both halves is load-bearing: it is the only way to
// close (not just narrow) the window in which a concurrent poll
// (ConfigStaleness, the watcher, sennit_info) could observe the write's new
// on-disk mtime against a snapshot that has not caught up yet and mistake
// this process's own write for an external change. fn reports wrote
// separately from err because a write that turns out to be a no-op (the key
// was already absent, or a precondition failed) must return err == nil
// without triggering a refresh — refreshing then would fold a real,
// concurrent external change into the snapshot and hide it from the next
// staleness check.
func (f *fileStaleness) withWrite(fn func() (wrote bool, err error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	wrote, err := fn()
	if wrote {
		f.refreshLocked(nil)
	}
	return err
}

// withWriteAddPath is withWrite for a write whose target path might not
// already be a tracked path: path is added to the tracked set (see
// addAndRefreshLocked) as part of the same locked refresh.
func (f *fileStaleness) withWriteAddPath(path string, fn func() (wrote bool, err error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	wrote, err := fn()
	if wrote {
		f.addAndRefreshLocked(path)
	}
	return err
}

func (f *fileStaleness) preReloadSnapshots() map[string]fileSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshots := make(map[string]fileSnapshot, len(f.tracked))
	for _, path := range f.tracked {
		snapshots[path] = statSnapshot(path)
	}
	return snapshots
}

func (f *fileStaleness) trackedPathSet() map[string]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	set := make(map[string]struct{}, len(f.tracked))
	for _, path := range f.tracked {
		set[path] = struct{}{}
	}
	return set
}

// ConfigStaleness checks whether tracked config files changed since the last
// snapshot. Its output is sorted and safe to read concurrently with refreshes.
func (s *ConfigStore) ConfigStaleness() StalenessResult { return s.staleness.check() }

// CaptureStalenessSnapshot tracks paths plus this store's active global and
// workspace paths. Callers that can race publication must hold writeMu while
// reading those store-owned paths, as existing mutation and reload paths do.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.staleness.capture(paths, []string{s.workspacePath.Get(), s.globalDataPath}, nil)
}
