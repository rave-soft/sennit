package runtime

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

type mapResolver map[string]string

func (r mapResolver) ResolveValue(value string) (string, error) {
	if resolved, ok := r[value]; ok {
		return resolved, nil
	}
	return value, nil
}

func accessToken(t *testing.T, account string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}})
	require.NoError(t, err)
	return fmt.Sprintf("x.%s.x", base64.RawURLEncoding.EncodeToString(payload))
}

func TestFromConfigSeparatesPersistedAndEffectiveValues(t *testing.T) {
	configured := config.ProviderConfig{
		ID:           "custom",
		APIKey:       "$KEY",
		ProxyURL:     "$PROXY",
		ExtraHeaders: map[string]string{"X-Test": "value"},
	}

	provider, err := FromConfig(configured, mapResolver{"$KEY": "secret", "$PROXY": "http://proxy.example:8080"})
	require.NoError(t, err)
	require.Equal(t, "secret", provider.APIKey)
	require.Equal(t, "$KEY", provider.APIKeyTemplate)
	require.Equal(t, "http://proxy.example:8080", provider.ProxyURL)
	require.Equal(t, provider.ProxyURL, provider.ConfiguredProxyURL)
	require.Equal(t, "$KEY", configured.APIKey)
	require.Equal(t, "$PROXY", configured.ProxyURL)

	provider.ExtraHeaders["X-Test"] = "changed"
	require.Equal(t, "value", configured.ExtraHeaders["X-Test"])
}

func TestProvidersHonorDisableDefaults(t *testing.T) {
	require.NotEmpty(t, Providers(&config.Config{Options: &config.Options{}}))
	require.Empty(t, Providers(&config.Config{Options: &config.Options{DisableDefaultProviders: true}}))
}

func TestApplyPostCredentialSetupAddsVendorHeaders(t *testing.T) {
	codexProvider := Provider{ID: codex.ProviderID, APIKey: accessToken(t, "account-a"), ExtraHeaders: map[string]string{"X-Mine": "keep"}}
	ApplyPostCredentialSetup(&codexProvider)
	require.Equal(t, "account-a", codexProvider.ExtraHeaders[codex.AccountIDHeader])
	require.Equal(t, "keep", codexProvider.ExtraHeaders["X-Mine"])

	copilotProvider := Provider{ID: string(catwalk.InferenceProviderCopilot)}
	require.NotPanics(t, func() { ApplyPostCredentialSetup(&copilotProvider) })
	require.NotEmpty(t, copilotProvider.ExtraHeaders)
}

type failingResolver struct{ err error }

func (r failingResolver) ResolveValue(string) (string, error) { return "", r.err }

func TestFromConfigPropagatesResolverError(t *testing.T) {
	want := errors.New("missing")
	_, err := FromConfig(config.ProviderConfig{ID: "openai", APIKey: "$MISSING"}, failingResolver{err: want})
	require.ErrorIs(t, err, want)
}

func TestApplyPostCredentialSetupRemovesStaleCodexAccount(t *testing.T) {
	provider := Provider{ID: codex.ProviderID, APIKey: accessToken(t, "account-a")}
	ApplyPostCredentialSetup(&provider)
	require.Equal(t, "account-a", provider.ExtraHeaders[codex.AccountIDHeader])

	provider.APIKey = "token-without-claims"
	provider.OAuthToken = &oauth.Token{AccessToken: "token-without-claims"}
	ApplyPostCredentialSetup(&provider)
	require.NotContains(t, provider.ExtraHeaders, codex.AccountIDHeader)
}

func TestToProviderCopiesIdentityAndModels(t *testing.T) {
	provider := ToProvider(Provider{ID: "custom", Name: "Custom", Models: []catwalk.Model{{ID: "model"}}})
	require.Equal(t, catwalk.InferenceProvider("custom"), provider.ID)
	require.Equal(t, "Custom", provider.Name)
	require.Equal(t, "model", provider.Models[0].ID)
}
