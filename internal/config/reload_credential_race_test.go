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
)

// racingRuntimeProcessor wraps testRuntimeProcessor and, on its first call
// after a store is attached, runs onProcess once. buildConfig invokes the
// processor before reloadFromDisk ever takes writeMu (see its doc comment),
// so firing the race from inside Process lands exactly in the window the
// real bug lived in: an account switch that completes while a reload's
// disk/HTTP work is still in flight, before reloadFromDisk snapshots
// s.CredentialVersion() against what it started with.
type racingRuntimeProcessor struct {
	store     *ConfigStore
	onProcess func(*ConfigStore)
	once      sync.Once
}

func (p *racingRuntimeProcessor) Process(ctx context.Context, input RuntimeInput) (RuntimeResult, error) {
	result, err := (testRuntimeProcessor{}).Process(ctx, input)
	if err == nil && p.store != nil {
		p.once.Do(func() {
			p.onProcess(p.store)
		})
	}
	return result, err
}

// TestReloadFromDisk_CredentialRacePreservesFullAccountSwitch is the
// regression test for the bug where a mid-reload credential race (an
// account switch landing while an autoReload/ReloadFromDisk's disk/HTTP
// work is still in flight) only preserved APIKey and OAuthToken, silently
// reverting Account, ProxyURL, and APIKeyTemplate back to whatever the
// stale disk read produced — and only re-ran Copilot's header setup, never
// Codex's, so a raced Codex switch lost its chatgpt-account-id header too.
//
// It seeds a real, disk-backed store (so reloadFromDisk's full pipeline
// runs), then uses racingRuntimeProcessor to run UpdateProviderAccount
// (simulating a concurrent account activation) from inside the reload's
// own processor callback — after reloadFromDisk has already snapshotted
// startCredentialVersion, but before it re-checks CredentialVersion() and
// runs the race-preservation block. Every field UpdateProviderAccount can
// publish must survive the reload that follows.
func TestReloadFromDisk_CredentialRacePreservesFullAccountSwitch(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	t.Setenv("SENNIT_GLOBAL_DATA", globalDir)

	oldToken := fakeCodexJWT(t, "acct-old")
	seed := fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {
    "mock": {"id": "mock", "name": "Mock", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]},
    "codex": {"id": "codex", "name": "Codex", "type": "openai",
      "base_url": "http://127.0.0.1:9/v1", "api_key": %q,
      "oauth": {"access_token": %q}, "proxy_url": "http://provider-proxy:8080",
      "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192}]}
  },
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`, oldToken, oldToken)
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, appName+".json"), []byte(seed), 0o644))

	workingDir := t.TempDir()
	processor := &racingRuntimeProcessor{}
	store, err := LoadWithProcessor(workingDir, "", false, processor)
	require.NoError(t, err)
	processor.store = store

	newToken := fakeCodexJWT(t, "acct-new")
	accountProxy := "http://account-proxy:9090"
	raced := AccountCredential{
		APIKey:          newToken,
		APIKeyTemplate:  "$RACE_TEST_TEMPLATE",
		Token:           &oauth.Token{AccessToken: newToken},
		ProxyURL:        &accountProxy,
		ActiveAccountID: "acct-new",
	}
	processor.onProcess = func(s *ConfigStore) {
		require.NoError(t, s.UpdateProviderAccount(codex.ProviderID, raced))
	}

	require.NoError(t, store.ReloadFromDisk(context.Background()))

	provider, ok := store.Config().Providers.Get(codex.ProviderID)
	require.True(t, ok)
	require.Equal(t, newToken, provider.APIKey, "APIKey must survive the race")
	require.NotNil(t, provider.OAuthToken)
	require.Equal(t, newToken, provider.OAuthToken.AccessToken, "OAuthToken must survive the race")
	require.Equal(t, "acct-new", provider.Account, "Account must survive the race")
	require.Equal(t, "http://account-proxy:9090", provider.ProxyURL, "ProxyURL must survive the race")
	require.Equal(t, "$RACE_TEST_TEMPLATE", provider.APIKeyTemplate, "APIKeyTemplate must survive the race")
	require.Equal(t, "acct-new", provider.ExtraHeaders["chatgpt-account-id"],
		"Codex's post-load setup must be re-run for the raced credential, not just Copilot's")
}
