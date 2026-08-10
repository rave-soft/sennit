package config

import (
	"context"
	"log/slog"
	"path/filepath"
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
	return false
}
