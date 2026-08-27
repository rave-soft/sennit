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

// TestMCPRemoveThenTwoAddsStaysFresh pins the fix for addLocal returning
// the tombstone wrapper map itself on a second "add" of an already-reset
// name: before the fix, this second call's fields landed beside
// __sennit_tombstone inside the wrapper, which ParseTombstone then
// rejects as an invalid tombstone (it requires exactly one field).
func TestMCPRemoveThenTwoAddsStaysFresh(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `mcp remove github
mcp add github --command node
mcp add github --env FOO bar`)

	entry := result["mcp"].(map[string]any)["github"].(map[string]any)
	marker := entry[TombstoneKey].(map[string]any)
	require.Len(t, marker, 3, "tombstone marker must only ever hold section/name/replacement")

	replacement := marker["replacement"].(map[string]any)
	require.Equal(t, "node", replacement["command"])
	require.Equal(t, "bar", replacement["env"].(map[string]any)["FOO"])
	require.NotContains(t, replacement, TombstoneKey)

	// Confirm the whole document still parses as a valid tombstone —
	// this is exactly what production's config loader does with it.
	tombstone, ok, err := ParseTombstone(entry, "mcp", "github")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "node", tombstone.Replacement["command"])
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
