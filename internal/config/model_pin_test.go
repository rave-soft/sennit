package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// twoProviderConfig is a config file naming large as the given
// provider/model, with both providers available so either choice resolves.
func twoProviderConfig(provider, model string) string {
	return `{
		"model": {"provider": "` + provider + `", "model": "` + model + `"},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"anthropic": {
				"api_key": "test-key-2",
				"models": [{"id": "claude-3", "name": "Claude 3"}]
			}
		}
	}`
}

// TestModelSelectionSurvivesPeerWrite is a regression test for the model
// switching out from under the user when several Sennit instances share the
// global config file. A sibling instance selecting a different model must
// not change ours, even though an unrelated config write (a token refresh,
// for example) reloads the file we both write to.
func TestModelSelectionSurvivesPeerWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// The user picks Claude in this instance.
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModel{
		Provider: "anthropic",
		Model:    "claude-3",
	}))

	// A sibling instance then picks GPT-4 and writes it to the shared file.
	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	// Any reload (here explicit; in practice triggered by an unrelated
	// write such as an OAuth token refresh) must leave our choice alone.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	large := store.Config().Model
	require.Equal(t, "anthropic", large.Provider)
	require.Equal(t, "claude-3", large.Model)
}

// TestModelSelectionSurvivesPeerWriteWhenUnchosen covers the case that
// actually bit: an instance running on the config's default model, never
// having picked one of its own, must still keep it when a sibling changes
// the default. Adopting it mid-session hands the session a model whose
// provider this instance may not even know about — which is how a model
// switch in one project broke a session running in another.
func TestModelSelectionSurvivesPeerWriteWhenUnchosen(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("anthropic", "claude-3")), 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	large := store.Config().Model
	require.Equal(t, "openai", large.Provider, "a running session keeps the model it started on")
	require.Equal(t, "gpt-4", large.Model)
}

// TestModelSelectionFollowsDiskOnNextStart is the other half of the rule:
// pinning lasts for the life of an instance, not beyond it, so changing the
// default in one client is picked up by the next client that starts.
func TestModelSelectionFollowsDiskOnNextStart(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	first, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.Equal(t, "openai", first.Config().Model.Provider)

	// A sibling instance switches the default and writes it out.
	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("anthropic", "claude-3")), 0o600))

	second, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.Equal(t, "anthropic", second.Config().Model.Provider, "a fresh start reads the file")
	require.Equal(t, "claude-3", second.Config().Model.Model)

	// And the instance that was already running is unaffected by it.
	require.NoError(t, first.ReloadFromDisk(context.Background()))
	require.Equal(t, "openai", first.Config().Model.Provider)
}

// TestModelSelectionStillFollowsOwnChoice: pinning must not stop this
// instance from changing its own model, which is the common case.
func TestModelSelectionStillFollowsOwnChoice(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "sennit.json")

	t.Setenv("SENNIT_GLOBAL_CONFIG", dir)
	t.Setenv("SENNIT_GLOBAL_DATA", dir)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("openai", "gpt-4")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModel{
		Provider: "anthropic",
		Model:    "claude-3",
	}))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	require.Equal(t, "anthropic", store.Config().Model.Provider)
	require.Equal(t, "claude-3", store.Config().Model.Model)
}
