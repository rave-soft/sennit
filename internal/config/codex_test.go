package config

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// codexAccessToken builds a token shaped like the one the Codex
// authorization server issues: a JWT naming the account it belongs to.
func codexAccessToken(t *testing.T, accountID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	})
	require.NoError(t, err)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

// TestCodexProviderInCatalog: Codex is absent from the embedded catwalk
// catalog, so Sennit has to contribute the entry itself — otherwise a
// signed-in user's provider is treated as an unknown custom one and dropped
// for having no base URL of its own.
func TestCodexProviderInCatalog(t *testing.T) {
	t.Parallel()

	providers := Providers(&Config{Options: &Options{}})

	idx := slices.IndexFunc(providers, func(p catwalk.Provider) bool {
		return string(p.ID) == codex.ProviderID
	})
	require.GreaterOrEqual(t, idx, 0, "the catalog must carry a codex entry")

	provider := providers[idx]
	require.Equal(t, catwalk.TypeOpenAI, provider.Type, "the Codex endpoint speaks the Responses API")
	require.Equal(t, codex.APIBaseURL, provider.APIEndpoint)
	require.Empty(t, provider.Models, "the model list is per-account and fetched at sign-in")
}

// TestCodexProviderSkippedWhenDefaultsDisabled keeps the codex entry on the
// same footing as every other catalog provider under
// disable_default_providers.
func TestCodexProviderSkippedWhenDefaultsDisabled(t *testing.T) {
	t.Parallel()

	providers := Providers(&Config{Options: &Options{DisableDefaultProviders: true}})
	require.Empty(t, providers)
}

// TestSetupCodexDerivesAccountFromToken: the account header is read out of
// the access token rather than stored, so a refresh cannot leave it stale.
func TestSetupCodexDerivesAccountFromToken(t *testing.T) {
	t.Parallel()

	pc := &ProviderConfig{APIKey: codexAccessToken(t, "acct-42")}
	require.NotPanics(t, pc.SetupCodex)

	require.Equal(t, "acct-42", pc.ExtraHeaders["chatgpt-account-id"])
	require.Equal(t, "codex_cli_rs", pc.ExtraHeaders["originator"])
}

// TestSetupCodexFallsBackToOAuthToken covers the window where the provider
// carries a token but no resolved api_key yet.
func TestSetupCodexFallsBackToOAuthToken(t *testing.T) {
	t.Parallel()

	pc := &ProviderConfig{
		OAuthToken: &oauth.Token{AccessToken: codexAccessToken(t, "acct-7")},
	}
	pc.SetupCodex()

	require.Equal(t, "acct-7", pc.ExtraHeaders["chatgpt-account-id"])
}

// TestSetupCodexKeepsExistingHeaders: a user's own extra_headers must
// survive the provider setup.
func TestSetupCodexKeepsExistingHeaders(t *testing.T) {
	t.Parallel()

	pc := &ProviderConfig{ExtraHeaders: map[string]string{"X-Mine": "keep"}}
	pc.SetupCodex()

	require.Equal(t, "keep", pc.ExtraHeaders["X-Mine"])
	require.NotContains(t, pc.ExtraHeaders, "chatgpt-account-id",
		"with no token there is no account to name, and an empty header would claim otherwise")
}
