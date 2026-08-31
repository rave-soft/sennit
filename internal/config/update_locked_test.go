package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigStoreUpdateLockedPublishesOnlyAfterSuccessfulWrite(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, &Config{})
	// A directory cannot be opened as the JSON output file, deterministically
	// causing the persistence path to fail without relying on permissions.
	store.globalDataPath = t.TempDir()
	before := store.Config()
	version := store.Version()

	err := store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
		c.Env = map[string]string{"UPDATE_LOCKED_TEST": "changed"}
		return map[string]any{"env.UPDATE_LOCKED_TEST": "changed"}
	})
	require.Error(t, err)
	require.Equal(t, before, store.Config(), "failed persistence must not publish the clone")
	require.Equal(t, version, store.Version(), "failed persistence must not advance config version")
}

func TestConfigStoreUpdateLockedWithoutFieldsPublishesInMemory(t *testing.T) {
	t.Parallel()
	store := newTestStore(t, &Config{})
	version := store.Version()

	require.NoError(t, store.updateLocked(ScopeGlobal, func(c *Config) map[string]any {
		c.Env = map[string]string{"UPDATE_LOCKED_TEST": "in-memory"}
		return nil
	}))
	require.Equal(t, "in-memory", store.Config().Env["UPDATE_LOCKED_TEST"])
	require.Greater(t, store.Version(), version, "no-write mutations still publish a new snapshot")
}
