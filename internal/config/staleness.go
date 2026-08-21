package config

import (
	"os"
	"path/filepath"
	"slices"
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

// ConfigStaleness checks whether any tracked config files have changed on disk
// since the last snapshot. Returns dirty=true if any files changed or went
// missing, along with sorted lists of affected paths. Stat errors are
// captured in Errors map but still treated as non-existence for dirty detection.
func (s *ConfigStore) ConfigStaleness() StalenessResult {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()

	var result StalenessResult
	result.Errors = make(map[string]error)

	for _, path := range s.trackedConfigPaths {
		snapshot, hadSnapshot := s.snapshots[path]

		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		if err != nil && !os.IsNotExist(err) {
			// Capture permission/IO errors separately from non-existence
			result.Errors[path] = err
			result.Dirty = true
		}

		if !exists {
			if hadSnapshot && snapshot.Exists {
				// File existed before but now missing
				result.Missing = append(result.Missing, path)
				result.Dirty = true
			}
			continue
		}

		// File exists now
		if !hadSnapshot || !snapshot.Exists {
			// File didn't exist before but does now
			result.Changed = append(result.Changed, path)
			result.Dirty = true
			continue
		}

		// Check for content or metadata changes
		if snapshot.Size != info.Size() || snapshot.ModTime != info.ModTime().UnixNano() {
			result.Changed = append(result.Changed, path)
			result.Dirty = true
		}
	}

	// Sort for deterministic output
	slices.Sort(result.Changed)
	slices.Sort(result.Missing)

	return result
}

// refreshStalenessSnapshotLocked captures fresh snapshots of all tracked
// config files. Caller must hold stalenessMu.
func (s *ConfigStore) refreshStalenessSnapshotLocked() {
	if s.snapshots == nil {
		s.snapshots = make(map[string]fileSnapshot)
	}

	for _, path := range s.trackedConfigPaths {
		info, err := os.Stat(path)
		exists := err == nil && !info.IsDir()

		snapshot := fileSnapshot{
			Path:   path,
			Exists: exists,
		}

		if exists {
			snapshot.Size = info.Size()
			snapshot.ModTime = info.ModTime().UnixNano()
		}

		s.snapshots[path] = snapshot
	}
}

// CaptureStalenessSnapshot captures snapshots for the given paths, building the
// tracked config paths list. Paths are deduplicated and normalized.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()

	// Build unique set of normalized paths
	seen := make(map[string]struct{})
	for _, p := range paths {
		if p == "" {
			continue
		}
		// Normalize path
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		seen[abs] = struct{}{}
	}

	// Also track workspace and global config paths if set
	if s.workspacePath != "" {
		abs, err := filepath.Abs(s.workspacePath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}
	if s.globalDataPath != "" {
		abs, err := filepath.Abs(s.globalDataPath)
		if err == nil {
			seen[abs] = struct{}{}
		}
	}

	// Build sorted list for deterministic ordering
	s.trackedConfigPaths = make([]string, 0, len(seen))
	for p := range seen {
		s.trackedConfigPaths = append(s.trackedConfigPaths, p)
	}
	slices.Sort(s.trackedConfigPaths)

	// Capture initial snapshots
	s.refreshStalenessSnapshotLocked()
}

// trackedConfigPathSet returns a copy of the currently tracked config paths
// as a set, for callers (externalChangeDetected in watch.go) that need to
// check membership without racing CaptureStalenessSnapshot/reloadFromDisk.
func (s *ConfigStore) trackedConfigPathSet() map[string]struct{} {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()
	set := make(map[string]struct{}, len(s.trackedConfigPaths))
	for _, p := range s.trackedConfigPaths {
		set[p] = struct{}{}
	}
	return set
}
