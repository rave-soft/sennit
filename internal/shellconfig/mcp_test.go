package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPRemove(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp add github --command npx
mcp add local --type http --url "http://localhost:3000/mcp"
mcp remove github`)

	mcps := result["mcp"].(map[string]any)
	github := mcps["github"].(map[string]any)
	require.Equal(t, map[string]any{"section": "mcp", "name": "github"}, github[TombstoneKey])
	require.Contains(t, mcps, "local")
}

func TestMCPRemoveAlias(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp add github --command npx
mcp rm github`)

	github := result["mcp"].(map[string]any)["github"].(map[string]any)
	require.Equal(t, map[string]any{"section": "mcp", "name": "github"}, github[TombstoneKey])
}

func TestMCPRemoveAbsentProducesTombstone(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp remove github`)
	entry := result["mcp"].(map[string]any)["github"].(map[string]any)
	require.Equal(t, map[string]any{"section": "mcp", "name": "github"}, entry[TombstoneKey])
}

func TestMCPRemoveThenAddIsFresh(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp remove github
mcp add github --command node`)
	entry := result["mcp"].(map[string]any)["github"].(map[string]any)
	marker := entry[TombstoneKey].(map[string]any)
	replacement := marker["replacement"].(map[string]any)
	require.Equal(t, "node", replacement["command"])
	require.NotContains(t, replacement, TombstoneKey)
}

func TestMCPOAuthFlags(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp add secure --type http --url "https://mcp.example.com" \
  --oauth true \
  --oauth-client-id "my-client" \
  --oauth-client-secret "my-secret" \
  --oauth-callback-port 8085`)

	m := result["mcp"].(map[string]any)["secure"].(map[string]any)
	require.Equal(t, true, m["oauth"])
	require.Equal(t, "my-client", m["oauth_client_id"])
	require.Equal(t, "my-secret", m["oauth_client_secret"])
	require.Equal(t, float64(8085), m["oauth_callback_port"])
}

func TestMCPUnknownSubcommand(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/sennitrc"
	_, err := LoadShellConfig(t.Context(), path, []byte(`mcp github --command npx`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown subcommand")
}
