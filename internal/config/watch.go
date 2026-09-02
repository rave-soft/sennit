package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// externalChangePollInterval is the production polling cadence.
const externalChangePollInterval = 2 * time.Second

// externalChangeWatcher owns watcher lifecycle state: polling configuration,
// callback registration, and the dynamic markdown-agent directory snapshot.
// Reloading and config publication remain ConfigStore responsibilities.
type externalChangeWatcher struct {
	pollInterval time.Duration // configured before WatchForExternalChanges starts

	callbackMu sync.Mutex
	callback   func()

	agentSnapshotMu sync.Mutex
	agentFiles      map[string]fileSnapshot
}

func (w *externalChangeWatcher) interval() time.Duration {
	if w.pollInterval > 0 {
		return w.pollInterval
	}
	return externalChangePollInterval
}

func (w *externalChangeWatcher) setCallback(fn func()) {
	w.callbackMu.Lock()
	w.callback = fn
	w.callbackMu.Unlock()
}

func (w *externalChangeWatcher) notify() {
	w.callbackMu.Lock()
	fn := w.callback
	w.callbackMu.Unlock()
	if fn != nil {
		fn()
	}
}

func (w *externalChangeWatcher) captureAgentFiles(workingDir string) {
	current := scanAgentFiles(workingDir)
	w.agentSnapshotMu.Lock()
	w.agentFiles = current
	w.agentSnapshotMu.Unlock()
}

func (w *externalChangeWatcher) agentFilesChanged(workingDir string) bool {
	current := scanAgentFiles(workingDir)
	w.agentSnapshotMu.Lock()
	defer w.agentSnapshotMu.Unlock()
	changed := len(current) != len(w.agentFiles)
	if !changed {
		for path, snapshot := range current {
			previous, ok := w.agentFiles[path]
			if !ok || previous.Size != snapshot.Size || previous.ModTime != snapshot.ModTime {
				changed = true
				break
			}
		}
	}
	w.agentFiles = current
	return changed
}

// OnExternalChange registers fn to run after a watcher-triggered reload. A
// later registration replaces the prior callback; callbacks run synchronously
// on the watcher goroutine.
func (s *ConfigStore) OnExternalChange(fn func()) { s.watcher.setCallback(fn) }

// WatchForExternalChanges polls files changed outside this process until ctx
// is cancelled. Process-local writes refresh fileStaleness while holding its
// mutex, so this loop only notifies after an external reload.
func (s *ConfigStore) WatchForExternalChanges(ctx context.Context) {
	if s.workingDir == "" {
		return
	}
	ticker := time.NewTicker(s.watcher.interval())
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
				s.CaptureStalenessSnapshot(lookupConfigs(s.workingDir))
				s.watcher.captureAgentFiles(s.workingDir)
				continue
			}
			s.watcher.notify()
		}
	}
}

func (s *ConfigStore) externalChangeDetected() bool {
	if s.ConfigStaleness().Dirty {
		return true
	}
	if s.hasUntrackedCandidate(lookupConfigs(s.workingDir)) {
		return true
	}
	return s.watcher.agentFilesChanged(s.workingDir)
}

func (s *ConfigStore) hasUntrackedCandidate(candidates []string) bool {
	tracked := s.trackedConfigPathSet()
	for _, path := range candidates {
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err == nil {
			if _, ok := tracked[abs]; !ok {
				return true
			}
		}
	}
	return false
}

func resolveAgentDirs(workingDir string) []string {
	dirs := make([]string, 0, len(agentDirs)+1)
	for _, dir := range agentDirs {
		dirs = append(dirs, filepath.Join(workingDir, dir))
	}
	return append(dirs, filepath.Join(filepath.Dir(GlobalConfig()), "agents"))
}

func scanAgentFiles(workingDir string) map[string]fileSnapshot {
	snapshot := make(map[string]fileSnapshot)
	for _, dir := range resolveAgentDirs(workingDir) {
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
			snapshot[path] = fileSnapshot{Path: path, Exists: true, Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		}
	}
	return snapshot
}
