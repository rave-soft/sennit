package workspace

import (
	"encoding/json"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/stretchr/testify/require"
)

// newFrontendConfigFixture builds a *config.Config with two of every trait
// NewFrontendConfig has to project: a catalog (non-custom) provider with a
// plain API key, a catalog provider with an OAuth token, a custom
// (base_url, not in the catalog) provider, and a disabled provider - plus
// recent models and a user-defined agent override, so every FrontendConfig
// method below has something real to read.
func newFrontendConfigFixture() (*config.Config, []catwalk.Provider) {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("openai", config.ProviderConfig{
		ID:      "openai",
		Name:    "OpenAI",
		BaseURL: "https://api.openai.com/v1",
		Type:    catwalk.TypeOpenAI,
		APIKey:  "sk-test",
		Models: []catwalk.Model{
			{ID: "gpt-5", Name: "GPT-5", CanReason: true, ReasoningLevels: []string{"low", "high"}},
		},
	})
	providers.Set("codex", config.ProviderConfig{
		ID:         "codex",
		Name:       "OpenAI Codex",
		BaseURL:    "https://chatgpt.com/backend-api/codex",
		Type:       catwalk.TypeOpenAI,
		OAuthToken: &oauth.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Account:    "acct-1",
		Models: []catwalk.Model{
			{ID: "codex-model", Name: "Codex"},
		},
	})
	providers.Set("myproxy", config.ProviderConfig{
		ID:       "myproxy",
		Name:     "My Proxy",
		BaseURL:  "https://proxy.example.com/v1",
		Type:     catwalk.TypeOpenAI,
		APIKey:   "sk-proxy",
		ProxyURL: "http://user:secret@proxy.internal:8080",
		Models: []catwalk.Model{
			{ID: "proxy-model", Name: "Proxy Model"},
		},
		Rotation: &config.RotationConfig{Enabled: true, Order: []string{"a", "b"}},
	})
	providers.Set("disabled-provider", config.ProviderConfig{
		ID:      "disabled-provider",
		Name:    "Disabled",
		Disable: true,
		Models:  []catwalk.Model{{ID: "d-model", Name: "D Model"}},
	})

	cfg := &config.Config{
		Model:        config.SelectedModel{Provider: "openai", Model: "gpt-5", ReasoningEffort: "high"},
		RecentModels: []config.SelectedModel{{Provider: "codex", Model: "codex-model", ReasoningEffort: "medium"}},
		Providers:    providers,
		MCP: config.MCPs{
			"docs":     config.MCPConfig{Type: config.MCPStdio, Command: "docs-server"},
			"disabled": config.MCPConfig{Type: config.MCPStdio, Command: "off-server", Disabled: true},
		},
		Agents: map[string]config.Agent{
			config.AgentCoder: {ID: config.AgentCoder},
			"reviewer":        {ID: "reviewer", Model: "openai/gpt-5", ReasoningEffort: "low"},
		},
		Options: &config.Options{
			InitializeAs:   "AGENTS.md",
			DisabledSkills: []string{"sennit-config"},
		},
	}
	cfg.SetRuntimeProvider("openai", providerstate.Provider{ID: "openai", APIKey: "sk-test"})
	cfg.SetRuntimeProvider("codex", providerstate.Provider{
		ID: "codex", OAuthToken: &oauth.Token{AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour).Unix()}, Account: "acct-1",
	})
	cfg.SetRuntimeProvider("myproxy", providerstate.Provider{ID: "myproxy", APIKey: "sk-proxy"})

	known := []catwalk.Provider{
		{ID: catwalk.InferenceProviderOpenAI, Name: "OpenAI", DefaultLargeModelID: "gpt-5"},
		{ID: catwalk.InferenceProvider("codex"), Name: "OpenAI Codex", DefaultLargeModelID: "codex-model"},
	}
	return cfg, known
}

// roundTrip JSON round-trips fc through encoding/json, the same as a
// Workspace served over gRPC with a JSON codec would.
func roundTrip(t *testing.T, fc *FrontendConfig) *FrontendConfig {
	t.Helper()
	data, err := json.Marshal(fc)
	require.NoError(t, err)
	var out FrontendConfig
	require.NoError(t, json.Unmarshal(data, &out))
	return &out
}

func TestNewFrontendConfig_ProviderProjection(t *testing.T) {
	t.Parallel()

	cfg, known := newFrontendConfigFixture()
	fc := NewFrontendConfig(cfg, known)
	rt := roundTrip(t, fc)

	for _, dto := range []*FrontendConfig{fc, rt} {
		require.Len(t, dto.Providers, 4)
		// Sorted by ID.
		var ids []string
		for _, p := range dto.Providers {
			ids = append(ids, p.ID)
		}
		require.Equal(t, []string{"codex", "disabled-provider", "myproxy", "openai"}, ids)

		openai, ok := dto.Provider("openai")
		require.True(t, ok)
		require.False(t, openai.Custom, "openai is in the catalog, not custom")
		require.True(t, openai.Auth.Known)
		require.True(t, openai.Auth.HasAPIKey)
		require.False(t, openai.Auth.HasOAuth)

		codex, ok := dto.Provider("codex")
		require.True(t, ok)
		require.False(t, codex.Custom)
		require.True(t, codex.Auth.HasOAuth)
		require.False(t, codex.Auth.Expired(time.Now().Unix()))
		require.Equal(t, "acct-1", codex.Auth.Account)

		myproxy, ok := dto.Provider("myproxy")
		require.True(t, ok)
		require.True(t, myproxy.Custom, "myproxy has a base_url and isn't in the catalog")
		require.NotContains(t, myproxy.ProxyURL, "secret", "the proxy password must not cross the wire")
		require.Contains(t, myproxy.ProxyURL, "user@", "the username may still be shown")
		require.NotNil(t, myproxy.Rotation)

		disabled, ok := dto.Provider("disabled-provider")
		require.True(t, ok)
		require.True(t, disabled.Disable)
		require.False(t, disabled.Auth.Known, "no runtime entry was ever set for this provider")

		_, ok = dto.Provider("does-not-exist")
		require.False(t, ok)
	}
}

// TestFrontendConfigMethods_MatchConfigConfig calls every method
// FrontendConfig carries on both the source *config.Config and the DTO
// (before and after a JSON round trip) and requires identical results -
// the contract this DTO exists to preserve for the UI call sites that
// used to call *config.Config directly.
func TestFrontendConfigMethods_MatchConfigConfig(t *testing.T) {
	t.Parallel()

	cfg, known := newFrontendConfigFixture()
	fc := NewFrontendConfig(cfg, known)
	rt := roundTrip(t, fc)

	for _, dto := range []*FrontendConfig{fc, rt} {
		require.Equal(t, cfg.GetModel("openai", "gpt-5"), dto.GetModel("openai", "gpt-5"))
		require.Nil(t, dto.GetModel("openai", "does-not-exist"))

		wantName, wantOK := cfg.ProviderName("openai")
		gotName, gotOK := dto.ProviderName("openai")
		require.Equal(t, wantOK, gotOK)
		require.Equal(t, wantName, gotName)

		require.Equal(t, cfg.SelectedCatalogModel(), dto.SelectedCatalogModel())
		require.Equal(t, cfg.GetProviderForModel().ID, dto.GetProviderForModel().ID)

		require.Equal(t, cfg.IsConfigured(), dto.IsConfigured())

		wantModel, wantEffort, wantOK := cfg.AgentOverride("reviewer")
		gotModel, gotEffort, gotOK := dto.AgentOverride("reviewer")
		require.Equal(t, wantOK, gotOK)
		require.Equal(t, wantModel, gotModel)
		require.Equal(t, wantEffort, gotEffort)
		// The built-in coder/task agents never carry an override.
		_, _, ok := dto.AgentOverride(config.AgentCoder)
		require.False(t, ok)

		require.ElementsMatch(t, cfg.MCPServerNames(), dto.MCPServerNames())

		require.Equal(t, cfg.RememberedReasoningEffort("openai", "gpt-5"), dto.RememberedReasoningEffort("openai", "gpt-5"))
		require.Equal(t, cfg.RememberedReasoningEffort("codex", "codex-model"), dto.RememberedReasoningEffort("codex", "codex-model"))
		require.Empty(t, dto.RememberedReasoningEffort("", "x"))

		wantDefault, wantErr := cfg.DefaultModelForProvider("openai", known)
		gotDefault, gotErr := dto.DefaultModelForProvider("openai", known)
		require.Equal(t, wantErr, gotErr)
		require.Equal(t, wantDefault, gotDefault)
		_, err := dto.DefaultModelForProvider("no-such-provider", known)
		require.Error(t, err)

		require.True(t, dto.HasCoderAgent())
		require.Equal(t, cfg.IsDockerMCPEnabled(), dto.IsDockerMCPEnabled())
	}
}

func TestFrontendConfig_HasCoderAgentFalseWithoutOne(t *testing.T) {
	t.Parallel()

	fc := &FrontendConfig{}
	require.False(t, fc.HasCoderAgent())
	var nilFC *FrontendConfig
	require.False(t, nilFC.HasCoderAgent())
}

// TestProviderAuth_States covers every classification the UI's provider
// settings dialog renders off ProviderAuth: no runtime entry, an API key,
// a valid OAuth token, an expired one, and the account field.
func TestProviderAuth_States(t *testing.T) {
	t.Parallel()

	cfg, known := newFrontendConfigFixture()
	// codex is a valid, unexpired OAuth token per the fixture; overwrite
	// it here with an expired one for that branch.
	cfg.SetRuntimeProvider("codex", providerstate.Provider{
		ID: "codex", OAuthToken: &oauth.Token{AccessToken: "at", ExpiresAt: time.Now().Add(-time.Hour).Unix()}, Account: "acct-1",
	})
	fc := NewFrontendConfig(cfg, known)

	unknown := fc.ProviderAuth("no-such-provider")
	require.False(t, unknown.Known)

	apiKey := fc.ProviderAuth("openai")
	require.True(t, apiKey.Known)
	require.True(t, apiKey.HasAPIKey)
	require.False(t, apiKey.HasOAuth)

	expired := fc.ProviderAuth("codex")
	require.True(t, expired.Known)
	require.True(t, expired.HasOAuth)
	require.True(t, expired.Expired(time.Now().Unix()))
	require.Equal(t, "acct-1", expired.Account)
}

func TestRedactProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no credentials", "http://proxy.internal:8080", "http://proxy.internal:8080"},
		{"user only", "http://user@proxy.internal:8080", "http://user@proxy.internal:8080"},
		{"user and password stripped", "http://user:secret@proxy.internal:8080", "http://user@proxy.internal:8080"},
		{"unparseable left alone", "$PROXY_URL", "$PROXY_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RedactProxyURL(tt.in)
			require.Equal(t, tt.want, got)
			require.NotContains(t, got, "secret")
		})
	}
}
