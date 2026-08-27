package skills

import (
	"context"
	"os"
	"sync"
	"time"
)

// ChangePollInterval is how often WatchForChanges checks discovery paths
// for SKILL.md files changed outside this process. Mirrors
// internal/config's externalChangePollInterval: skill edits are rare and
// not latency-sensitive, so a cheap poll beats fsnotify's trouble with
// directories that don't exist yet (a project's first .sennit/skills) and
// editors' atomic rename-replace saves. A var, not a const, so tests can
// shorten it.
var ChangePollInterval = 2 * time.Second

// skillFileSnapshot is the size+mtime pair WatchForChanges diffs between
// polls, at the same granularity config.fileSnapshot uses for config files.
type skillFileSnapshot struct {
	size    int64
	modTime int64
}

// scanSkillFiles walks every discovery path — the same traversal
// DiscoverWithStates uses — and snapshots each SKILL.md file's size and
// mtime. A path that doesn't exist yet is not an error: a project's first
// skills directory is typically created mid-session, same as agentDirs in
// internal/config.
func scanSkillFiles(paths []string) map[string]skillFileSnapshot {
	snapshot := make(map[string]skillFileSnapshot)
	var mu sync.Mutex
	walkSkillFiles(paths, func(_, path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// fastwalk is concurrent, so we protect the shared snapshot
		// map with mu.
		mu.Lock()
		snapshot[path] = skillFileSnapshot{size: info.Size(), modTime: info.ModTime().UnixNano()}
		mu.Unlock()
		return nil
	}, nil)
	return snapshot
}

// skillSnapshotsDiffer reports whether any SKILL.md file was added,
// removed, or edited between two scans.
func skillSnapshotsDiffer(prev, current map[string]skillFileSnapshot) bool {
	if len(prev) != len(current) {
		return true
	}
	for path, snap := range current {
		if prevSnap, ok := prev[path]; !ok || prevSnap != snap {
			return true
		}
	}
	return false
}

// WatchForChanges polls cfg's discovery paths for SKILL.md files added,
// edited, or removed outside this process (an agent's Edit/Write tool, or
// a human editing one directly) and, on a detected change, re-runs
// DiscoverFromConfig and swaps the result into mgr via
// Manager.ReplaceDiscovery, then invokes onChange. It returns once ctx is
// done; callers should run it in its own goroutine, one per workspace —
// the same shape as config.ConfigStore.WatchForExternalChanges, which this
// mirrors so the two hot-reload mechanisms behave consistently.
//
// interval overrides ChangePollInterval when positive; tests use this to
// avoid a real multi-second wait.
// cfg is a function, not a value, because the config is not static: the
// inherited-skill set a child workspace carries can be replaced by its
// parent between polls, and each pass must discover against the current
// one.
func WatchForChanges(ctx context.Context, cfg func() DiscoveryConfig, mgr *Manager, interval time.Duration, onChange func()) {
	if mgr == nil || cfg == nil {
		return
	}
	if interval <= 0 {
		interval = ChangePollInterval
	}

	last := scanSkillFiles(cfg().ResolvePaths())

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			discoveryCfg := cfg()
			current := scanSkillFiles(discoveryCfg.ResolvePaths())
			if !skillSnapshotsDiffer(last, current) {
				continue
			}
			last = current

			allSkills, activeSkills, states := DiscoverFromConfig(discoveryCfg)
			mgr.ReplaceDiscovery(allSkills, activeSkills, states)
			if onChange != nil {
				onChange()
			}
		}
	}
}
