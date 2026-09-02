package config

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/state"
)

// TestConfigStore_CredentialWritesShareWriteMuWithConfigMutators protects
// the invariant credential_store.go's file comment describes: a credential
// publish (UpdateProviderAccount, here through UpdateProviderCredentials)
// and an ordinary in-memory config mutator (OverridePreferredModel) both
// go through the same writeMu and must serialise on it rather than run
// concurrently. A second, credential-only mutex guarding the same *Config
// pointer would not be an optimisation — it would let these two goroutines
// race on cfg.cloneForWrite()/setConfig(), which is exactly the failure
// writeMu exists to rule out. Run with -race, which would catch that race
// long before either goroutine group finishes.
func TestConfigStore_CredentialWritesShareWriteMuWithConfigMutators(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set(codex.ProviderID, ProviderConfig{ID: codex.ProviderID})
	runtimeProviders := csync.NewMap[string, state.Provider]()
	runtimeProviders.Set(codex.ProviderID, state.Provider{ID: codex.ProviderID})
	store := &ConfigStore{
		config:    &Config{Providers: providers, RuntimeProviders: runtimeProviders},
		processor: testRuntimeProcessor{},
	}

	const iterations = 50
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range iterations {
			apiKey := "key-" + string(rune('a'+i%26))
			require.NoError(t, store.UpdateProviderCredentials(codex.ProviderID, apiKey, nil))
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			store.OverridePreferredModel(SelectedModel{Provider: codex.ProviderID, Model: "gpt"})
		}
	}()
	wg.Wait()

	provider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.NotEmpty(t, provider.APIKey)
	require.Equal(t, SelectedModel{Provider: codex.ProviderID, Model: "gpt"}, store.Config().Model)
	require.Greater(t, store.CredentialVersion(), uint64(0))
}
