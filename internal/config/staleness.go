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
	s.captureStalenessSnapshot(paths, nil)
}

func (s *ConfigStore) captureStalenessSnapshot(paths []string, preRead map[string]fileSnapshot) {
	s.staleness.capture(paths, []string{s.workspacePath.Get(), s.globalDataPath}, preRead)
}

func (s *ConfigStore) preReloadFileSnapshots() map[string]fileSnapshot {
	return s.staleness.preReloadSnapshots()
}

func (s *ConfigStore) trackedConfigPathSet() map[string]struct{} {
	return s.staleness.trackedPathSet()
}
