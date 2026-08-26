package config

import (
	"path/filepath"
	"testing"
)

type testStoreOption func(*StoreOptions)

func withGlobalDataPath(path string) testStoreOption {
	return func(options *StoreOptions) { options.GlobalDataPath = path }
}

func newTestStore(tb testing.TB, cfg *Config, opts ...testStoreOption) *ConfigStore {
	tb.Helper()
	options := StoreOptions{
		Config:         cfg,
		GlobalDataPath: filepath.Join(tb.TempDir(), appName+".json"),
	}
	for _, opt := range opts {
		opt(&options)
	}
	return NewStore(options)
}
