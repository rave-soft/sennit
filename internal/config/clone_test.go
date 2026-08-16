package config

import (
	"testing"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/stretchr/testify/require"
)

// TestCloneForWrite_Isolation verifies that mutating a clone never reaches
// back into the original Config. This is the contract the store's
// copy-on-write mutators depend on for race-free publishing.
func TestCloneForWrite_Isolation(t *testing.T) {
	t.Parallel()

	orig := &Config{
		Model:        SelectedModel{Provider: "openai", Model: "gpt-4"},
		RecentModels: []SelectedModel{{Provider: "openai", Model: "gpt-4"}},
		MCP:          MCPs{"a": {}},
		Providers: csync.NewMap(map[string]ProviderConfig{
			"openai": {APIKey: "old", OAuthToken: &oauth.Token{AccessToken: "old", Client: &oauth.OAuthClient{ClientID: "old"}}},
		}),
		Options: &Options{
			TUI: &TUIOptions{CompactMode: false},
		},
	}

	clone := orig.cloneForWrite()

	// Mutate every field the typed mutators touch.
	clone.Model = SelectedModel{Provider: "anthropic", Model: "claude"}
	clone.RecentModels[0] = SelectedModel{Provider: "anthropic", Model: "claude"}
	clone.MCP["b"] = MCPConfig{}
	clone.Options.TUI.CompactMode = true
	provider, ok := clone.Providers.Get("openai")
	require.True(t, ok)
	provider.APIKey = "new"
	provider.OAuthToken.AccessToken = "new"
	provider.OAuthToken.Client.ClientID = "new"
	clone.Providers.Set("openai", provider)
	enabled := true
	clone.Options.TUI.Transparent = &enabled

	// The original must be untouched.
	require.Equal(t, "openai", orig.Model.Provider, "Model leaked")
	require.Equal(t, "openai", orig.RecentModels[0].Provider, "RecentModels leaked")
	require.NotContains(t, orig.MCP, "b", "MCP leaked")
	originalProvider, ok := orig.Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "old", originalProvider.APIKey)
	require.Equal(t, "old", originalProvider.OAuthToken.AccessToken)
	require.Equal(t, "old", originalProvider.OAuthToken.Client.ClientID)
	require.False(t, orig.Options.TUI.CompactMode, "Options.TUI.CompactMode leaked")
	require.Nil(t, orig.Options.TUI.Transparent, "Options.TUI.Transparent leaked")
}
