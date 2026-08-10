package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// externalChangePollInterval is how often WatchForExternalChanges checks
// tracked config files for changes made outside this process. Config edits
// are rare and not latency-sensitive, so a cheap poll beats the complexity
// of watching directories that may not exist yet (a project's first
// .braid/, say) or of teasing apart editors' atomic rename-replace saves
// from a real fsnotify watch.
const externalChangePollInterval = 2 * time.Second

// OnExternalChange registers fn to run after WatchForExternalChanges
// reloads config because of a change made outside this process — for
// example an agent's Edit/Write tool touching .braid/braid.json directly,
// instead of going through SetConfigFields. Only one callback is kept; a
// later call replaces the previous one. fn runs synchronously on the
// watcher goroutine, so it should not block; callers that need to touch
// shared state (like backend's publishConfigChanged, which re-inits MCP
// servers and publishes a ConfigChanged event) should dispatch async work
// themselves rather than block the poll loop.
//
// A reload triggered by this process's own writes (SetConfigFields,
// the typed mutators, ...) does not run fn — those callers already know
// what changed and publish their own notifications.
func (s *ConfigStore) OnExternalChange(fn func()) {
	s.onExternalChangeMu.Lock()
	s.onExternalChange = fn
	s.onExternalChangeMu.Unlock()
}

// WatchForExternalChanges polls tracked config files for changes made
// outside this process and reloads config when one is detected. It returns
// once ctx is done; callers should run it in its own goroutine, one per
// workspace.
//
// Detection relies on the same staleness snapshot ConfigStaleness reports
// from. Every reload this process triggers itself — including the
// autoReload that follows SetConfigFields — refreshes that snapshot before
// returning (see the "Refresh the staleness snapshot" comment on
// updateLocked), so this loop only ever fires for genuine external edits,
// never for its own writes.
func (s *ConfigStore) WatchForExternalChanges(ctx context.Context) {
	if s.workingDir == "" {
		return
	}

	ticker := time.NewTicker(externalChangePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.externalChangeDetected() {
				continue
			}
			if err := s.ReloadFromDisk(ctx); err != nil {
				slog.Warn("Failed to reload config after external change", "error", err)
				continue
			}
			s.onExternalChangeMu.Lock()
			fn := s.onExternalChange
			s.onExternalChangeMu.Unlock()
			if fn != nil {
				fn()
			}
		}
	}
}

// externalChangeDetected reports whether any config file this process
// knows about has changed on disk since the last reload.
//
// ConfigStaleness alone is not quite enough: it only tracks paths that
// were already candidates as of the last snapshot, so a config file
// created for the first time after that snapshot (a project's first
// .braid/braid.json, written by an agent's Write tool mid-session) is
// invisible to it. Re-running the candidate walk catches that case too;
// it is cheap (stat calls only, no file reads).
func (s *ConfigStore) externalChangeDetected() bool {
	if s.ConfigStaleness().Dirty {
		return true
	}

	tracked := s.trackedConfigPathSet()
	for _, p := range lookupConfigs(s.workingDir) {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		if _, ok := tracked[abs]; !ok {
			return true
		}
	}

	// Subagent markdown files (agentDirs, agents_markdown.go) live under
	// directories rather than a fixed path list, so they need their own
	// added/removed/edited check alongside the config-file one above.
	if s.agentFilesChanged() {
		return true
	}

	return false
}

// resolveAgentDirs returns every directory scanned for markdown agent
// files, in the same order discoverMarkdownAgents uses, resolved against
// this store's working directory.
func (s *ConfigStore) resolveAgentDirs() []string {
	dirs := make([]string, 0, len(agentDirs)+1)
	for _, dir := range agentDirs {
		dirs = append(dirs, filepath.Join(s.workingDir, dir))
	}
	dirs = append(dirs, filepath.Join(filepath.Dir(GlobalConfig()), "agents"))
	return dirs
}

// scanAgentFiles snapshots every *.md file under the agent directories —
// path, size, and mtime, at the same granularity fileSnapshot already uses
// for config files. A missing directory is skipped rather than treated as
// an error: a project's first .braid/agents is typically created mid-
// session, and a global agents directory may never exist at all.
func (s *ConfigStore) scanAgentFiles() map[string]fileSnapshot {
	snapshot := make(map[string]fileSnapshot)
	for _, dir := range s.resolveAgentDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			snapshot[path] = fileSnapshot{
				Path:    path,
				Exists:  true,
				Size:    info.Size(),
				ModTime: info.ModTime().UnixNano(),
			}
		}
	}
	return snapshot
}

// captureAgentFileSnapshot records the current state of agent files
// without reporting a diff — used right after a (re)load, mirroring
// captureStalenessSnapshot, so the first watcher poll compares against
// what was actually loaded rather than against nothing.
func (s *ConfigStore) captureAgentFileSnapshot() {
	current := s.scanAgentFiles()
	s.agentSnapshotMu.Lock()
	s.agentFileSnapshot = current
	s.agentSnapshotMu.Unlock()
}

// agentFilesChanged reports whether any agent markdown file was added,
// removed, or edited since the last snapshot, and refreshes the snapshot
// as a side effect. Folding the refresh in here (rather than a separate
// call, as ConfigStaleness/RefreshStalenessSnapshot do) is safe because
// the poll loop is the only caller and always wants both together.
func (s *ConfigStore) agentFilesChanged() bool {
	current := s.scanAgentFiles()

	s.agentSnapshotMu.Lock()
	defer s.agentSnapshotMu.Unlock()

	changed := len(current) != len(s.agentFileSnapshot)
	if !changed {
		for path, snap := range current {
			prev, ok := s.agentFileSnapshot[path]
			if !ok || prev.Size != snap.Size || prev.ModTime != snap.ModTime {
				changed = true
				break
			}
		}
	}
	s.agentFileSnapshot = current
	return changed
}
