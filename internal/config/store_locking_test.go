package config

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/brand"
	"github.com/stretchr/testify/require"
)

// TestConfigStore_ConcurrentReadDuringReload exercises the writeMu/configMu
// pairing that lets Config() readers run concurrently with a reload: the
// reload does its disk I/O and provider setup before ever touching writeMu,
// then swaps in a fresh Config under a brief writeMu.Lock/configMu.Lock. A
// reader taking configMu.RLock via Config() must never observe a torn or
// partially-built Config, and must never deadlock against the reload. Run
// with -race.
func TestConfigStore_ConcurrentReadDuringReload(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv(brand.EnvPrefix+"GLOBAL_CONFIG", dir)
	t.Setenv(brand.EnvPrefix+"GLOBAL_DATA", dir)

	cfg := `{
		"model": {"provider": "openai", "model": "gpt-4"},
		"providers": {"openai": {"api_key": "k", "models": [{"id": "gpt-4", "name": "GPT-4"}]}}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(cfg), 0o600))

	store, err := LoadData(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: walk the config pointer and a nested field, the way a
	// runtime compiling a provider client would.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c := store.Config()
					require.NotNil(t, c)
					if c.Providers != nil {
						_, _ = c.Providers.Get("openai")
					}
				}
			}
		}()
	}

	// Writer: reloads from disk repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 30 {
			require.NoError(t, store.ReloadFromDisk(context.Background()))
		}
		close(stop)
	}()

	wg.Wait()
}

// TestConfigStore_StalenessSnapshotRacesWrite exercises stalenessMu against
// a concurrent SetConfigFields write: ConfigStaleness (called without
// writeMu held, e.g. from the sennit_info tool or the external-change poll
// loop) must not race the writeMu-held snapshot recapture that follows a
// write's auto-reload. Run with -race.
func TestConfigStore_StalenessSnapshotRacesWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv(brand.EnvPrefix+"GLOBAL_CONFIG", dir)
	t.Setenv(brand.EnvPrefix+"GLOBAL_DATA", dir)
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))

	store, err := LoadData(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader: repeatedly checks staleness, as WatchForExternalChanges does.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = store.ConfigStaleness()
			}
		}
	}()

	// Writer: mutates a field, which writes to disk and then reloads,
	// recapturing the staleness snapshot under writeMu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", i%2 == 0))
		}
		close(stop)
	}()

	wg.Wait()
}
