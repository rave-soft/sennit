// Package configtest assembles [config.ConfigStore]s for tests. Production
// code builds a store through the real load pipeline (config.LoadData /
// configruntime.Load), which reads the config files from disk and records
// everything a store needs; tests that exercise a store without a real
// config on disk build one here instead, against a hand-made
// [config.Config], so the testing.TB wrapper and the per-test defaults live
// in exactly one place. It wraps the architecture-neutral production
// constructor [config.NewStore] and adds only testing conveniences: the
// per-test temp-dir global data path and the testing.TB plumbing. It
// imports config one-way, and only _test.go files import this package, so
// the production config graph stays free of the testing package.
package configtest

import (
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
)

// Option customizes the config.StoreOptions and the construction metadata
// a [NewStore] store is built with.
type Option func(*config.StoreOptions)

// WithLoadedPaths sets the store's loaded config file paths, as the
// production load would have recorded them. Tests that assert on
// ConfigStore.LoadedPaths (the sennit_info dump, staleness tracking) need
// the value to be realistic rather than empty.
func WithLoadedPaths(paths ...string) Option {
	return func(options *config.StoreOptions) { options.LoadedPaths = paths }
}

// WithWorkingDir sets the store's working directory. Callers whose code
// paths depend on it (e.g. resolving relative context paths) need this;
// [NewStore] otherwise leaves it empty.
func WithWorkingDir(workingDir string) Option {
	return func(options *config.StoreOptions) { options.WorkingDir = workingDir }
}

// WithGlobalDataPath overrides the store's global data path, the file that
// global writes and staleness checks are scoped against. [NewStore] already
// anchors this at a per-test temp dir; use this option only when a test
// needs a specific location (e.g. a shared dir across two stores).
func WithGlobalDataPath(path string) Option {
	return func(options *config.StoreOptions) { options.GlobalDataPath = path }
}

// NewStore assembles a config.ConfigStore through the production
// constructor [config.NewStore]: the shell variable resolver over the
// process environment, the production external-change poll interval, and —
// unlike the production constructor's empty default — a global data path
// anchored at a per-test file under tb's temp dir, mirroring what the
// production load would set (config.GlobalConfigData). Anchoring it to a
// real, writable, per-test path (rather than leaving it empty) keeps tests
// from depending on the empty-path behavior production guards against but
// never itself exercises. Options apply after the defaults, so
// [WithGlobalDataPath] can override the per-test anchor.
func NewStore(tb testing.TB, cfg *config.Config, opts ...Option) *config.ConfigStore {
	tb.Helper()
	options := config.StoreOptions{
		Config:         cfg,
		GlobalDataPath: filepath.Join(tb.TempDir(), brand.Slug+".json"),
	}
	for _, opt := range opts {
		opt(&options)
	}
	return config.NewStore(options)
}
