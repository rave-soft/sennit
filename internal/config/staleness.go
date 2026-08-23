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

// statSnapshot stats a single path and reports its current fileSnapshot.
func statSnapshot(path string) fileSnapshot {
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

	return snapshot
}

// refreshStalenessSnapshotLocked captures snapshots of all tracked config
// files. preRead, if non-nil, supplies the snapshot for any path already
// present in it instead of statting the path fresh — see
// captureStalenessSnapshot for why reloadFromDisk needs this. Caller must
// hold stalenessMu.
func (s *ConfigStore) refreshStalenessSnapshotLocked(preRead map[string]fileSnapshot) {
	if s.snapshots == nil {
		s.snapshots = make(map[string]fileSnapshot)
	}

	for _, path := range s.trackedConfigPaths {
		if snapshot, ok := preRead[path]; ok {
			s.snapshots[path] = snapshot
			continue
		}
		s.snapshots[path] = statSnapshot(path)
	}
}

// CaptureStalenessSnapshot captures snapshots for the given paths, building the
// tracked config paths list. Paths are deduplicated and normalized.
func (s *ConfigStore) CaptureStalenessSnapshot(paths []string) {
	s.captureStalenessSnapshot(paths, nil)
}

// preReloadFileSnapshots stats every currently tracked config path and
// returns their snapshots. reloadFromDisk calls this before buildConfig
// re-reads file contents, then feeds the result back into
// captureStalenessSnapshot as preRead once the reload is done. Without
// this, a write landing after the files were read but before the reload
// finishes would get its fresh-at-swap-time mtime/size recorded even
// though its new content was never loaded, and the change would be
// silently absorbed instead of showing up as stale on the next check.
func (s *ConfigStore) preReloadFileSnapshots() map[string]fileSnapshot {
	s.stalenessMu.Lock()
	defer s.stalenessMu.Unlock()

	snapshots := make(map[string]fileSnapshot, len(s.trackedConfigPaths))
	for _, path := range s.trackedConfigPaths {
		snapshots[path] = statSnapshot(path)
	}
	return snapshots
}

// captureStalenessSnapshot is CaptureStalenessSnapshot's implementation.
// preRead lets a caller (reloadFromDisk) supply snapshots taken before this
// call for paths it already knew about; only paths first discovered by
// this call (i.e. absent from preRead) are stat'd here. A nil preRead
// stats every path fresh, which is CaptureStalenessSnapshot's documented
// behaviour for its other callers.
func (s *ConfigStore) captureStalenessSnapshot(paths []string, preRead map[string]fileSnapshot) {
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
	if workspacePath := s.workspacePath.Get(); workspacePath != "" {
		abs, err := filepath.Abs(workspacePath)
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
	s.refreshStalenessSnapshotLocked(preRead)
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
