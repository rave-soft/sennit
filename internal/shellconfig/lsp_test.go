package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLSPRemove(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `lsp add gopls --command gopls
lsp add pyright --command pyright-langserver
lsp remove gopls`)

	lsps := result["lsp"].(map[string]any)
	gopls := lsps["gopls"].(map[string]any)
	require.Equal(t, map[string]any{"section": "lsp", "name": "gopls"}, gopls[TombstoneKey])
	require.Contains(t, lsps, "pyright")
}

func TestLSPRemoveAlias(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `lsp add gopls --command gopls
lsp rm gopls`)

	gopls := result["lsp"].(map[string]any)["gopls"].(map[string]any)
	require.Equal(t, map[string]any{"section": "lsp", "name": "gopls"}, gopls[TombstoneKey])
}

func TestLSPRemoveAbsentAndThenAdd(t *testing.T) {
	t.Parallel()

	removed := loadScript(t, `lsp remove gopls`)
	marker := removed["lsp"].(map[string]any)["gopls"].(map[string]any)
	require.Equal(t, map[string]any{"section": "lsp", "name": "gopls"}, marker[TombstoneKey])

	readded := loadScript(t, `lsp remove gopls
lsp add gopls --command fresh`)
	entry := readded["lsp"].(map[string]any)["gopls"].(map[string]any)
	marker = entry[TombstoneKey].(map[string]any)
	replacement := marker["replacement"].(map[string]any)
	require.Equal(t, "fresh", replacement["command"])
	require.NotContains(t, replacement, TombstoneKey)
}

func TestLSPUnknownSubcommand(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/sennitrc"
	_, err := LoadShellConfig(t.Context(), path, []byte(`lsp gopls --command gopls`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown subcommand")
}
