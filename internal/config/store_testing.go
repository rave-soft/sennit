package config

import (
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/env"
)

// TestStoreOption customizes a [ConfigStore] built by [NewTestStore].
type TestStoreOption func(*ConfigStore)

// WithLoadedPaths sets the store's loadedPaths, as Load would have recorded
// them.
func WithLoadedPaths(paths ...string) TestStoreOption {
	return func(s *ConfigStore) { s.loadedPaths = paths }
}

// WithWorkingDir sets the store's working directory. Callers whose code
// paths depend on it (e.g. resolving relative context paths) need this;
// NewTestStore otherwise leaves it empty.
func WithWorkingDir(workingDir string) TestStoreOption {
	return func(s *ConfigStore) { s.workingDir = workingDir }
}

// WithGlobalDataPath overrides the store's globalDataPath, the file that
// global writes and staleness checks are scoped against. NewTestStore
// already anchors this at a per-test temp dir; use this option only when a
// test needs a specific location (e.g. a shared dir across two stores).
func WithGlobalDataPath(path string) TestStoreOption {
	return func(s *ConfigStore) { s.globalDataPath = path }
}

// NewTestStore creates a ConfigStore for testing purposes. Its
// globalDataPath defaults to a config.json under tb's temp dir, mirroring
// what Load would set in production (see GlobalConfigData); tests that need
// a specific path can override it with WithGlobalDataPath. Anchoring it to
// a real, writable, per-test path (rather than leaving it empty) keeps
// tests from depending on the empty-path behavior that production guards
// against but never itself exercises.
func NewTestStore(tb testing.TB, cfg *Config, opts ...TestStoreOption) *ConfigStore {
	tb.Helper()
	s := &ConfigStore{
		config:                     cfg,
		resolver:                   NewShellVariableResolver(env.New()),
		externalChangePollInterval: externalChangePollInterval,
		globalDataPath:             filepath.Join(tb.TempDir(), appName+".json"),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
