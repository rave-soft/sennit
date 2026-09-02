package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/state"
)

// racingOtherProviderProcessor is racingRuntimeProcessor's twin: it fires
// onProcess from inside buildConfig's Process call — the same window a
// disk-triggered reload spends between snapshotting startCredentialVersion
// and re-checking it — but the credential update it fires touches a
// *different* provider than the one whose disk config this test edits.
type racingOtherProviderProcessor struct {
	store     *ConfigStore
	onProcess func(*ConfigStore)
	once      sync.Once
}

func (p *racingOtherProviderProcessor) CompileProvider(configured ProviderConfig, resolver VariableResolver) (state.Provider, error) {
	return (testRuntimeProcessor{}).CompileProvider(configured, resolver)
}

func (p *racingOtherProviderProcessor) ApplyProviderCredentials(provider state.Provider) (state.Provider, error) {
	return (testRuntimeProcessor{}).ApplyProviderCredentials(provider)
}

func (p *racingOtherProviderProcessor) Process(ctx context.Context, input RuntimeInput) (RuntimeResult, error) {
	result, err := (testRuntimeProcessor{}).Process(ctx, input)
	if err == nil && p.store != nil {
		p.once.Do(func() {
			p.onProcess(p.store)
		})
	}
	return result, err
}

// TestReloadFromDisk_CredentialRaceOnOtherProviderKeepsFreshDiskState is the
// regression test for the second bug in this pass: reload.go's race-guard
// block used to copy *every* provider's pre-reload RuntimeProviders entry
// forward whenever CredentialVersion had moved, not just the one the racing
// publish actually touched. That meant a base_url edit landing in the same
// reload as an unrelated provider's credential refresh was silently
// discarded — the disk-shaped Providers map got the new base_url, but
// RuntimeProviders (what requests actually use) kept serving the old one
// until a further reload happened to run uncontested.
//
// This seeds two providers ("mock" and "codex"), edits mock's base_url on
// disk, then reloads while racingOtherProviderProcessor fires an account
// switch on codex from inside the reload's own processor callback — after
// startCredentialVersion is captured but before the race-guard re-checks it.
// mock's reloaded base_url must survive (nothing touched its credentials);
// codex's raced credentials must also survive (that's the existing,
// already-tested case — reload_credential_race_test.go covers it on its
// own, this just asserts it isn't broken by scoping the copy down).
func TestReloadFromDisk_CredentialRaceOnOtherProviderKeepsFreshDiskState(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	oldToken := fakeCodexJWT(t, "acct-old")
	configPath := filepath.Join(globalDir, appName+".json")
	seed := func(mockBaseURL string) string {
		return fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {
    "mock": {"id": "mock", "name": "Mock", "type": "openai",
      "base_url": %q, "api_key": "test-key",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]},
    "codex": {"id": "codex", "name": "Codex", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": %q,
      "oauth": {"access_token": %q}, "proxy_url": "http://provider-proxy:8080",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}
  },
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, mockBaseURL, oldToken, oldToken)
	}
	require.NoError(t, os.WriteFile(configPath, []byte(seed("http://127.0.0.1:9/v1")), 0o644))

	workingDir := t.TempDir()
	processor := &racingOtherProviderProcessor{}
	store, err := LoadWithProcessor(workingDir, "", false, processor)
	require.NoError(t, err)
	processor.store = store

	before, ok := store.Config().RuntimeProvider("mock")
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:9/v1", before.BaseURL)

	// Edit mock's base_url on disk — the change this reload is meant to
	// pick up.
	require.NoError(t, os.WriteFile(configPath, []byte(seed("http://127.0.0.1:9999/v1")), 0o644))

	// Race an account switch on codex — a different provider — from inside
	// the reload's own processor callback.
	newToken := fakeCodexJWT(t, "acct-new")
	raced := AccountCredential{
		APIKey:          newToken,
		Token:           &oauth.Token{AccessToken: newToken},
		ActiveAccountID: "acct-new",
	}
	processor.onProcess = func(s *ConfigStore) {
		require.NoError(t, s.UpdateProviderAccount(codex.ProviderID, raced))
	}

	require.NoError(t, store.ReloadFromDisk(context.Background()))

	mock, ok := store.Config().RuntimeProvider("mock")
	require.True(t, ok)
	require.Equal(t, "http://127.0.0.1:9999/v1", mock.BaseURL,
		"mock's freshly reloaded base_url must survive — the race touched codex, not mock")

	codexProvider, ok := store.Config().RuntimeProvider(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, codexProvider.APIKey, "codex's raced credential must still survive")
	require.Equal(t, "acct-new", codexProvider.Account)
}
