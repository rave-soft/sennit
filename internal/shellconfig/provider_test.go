package shellconfig

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProviderRemoveThenAdd guards against a bug where `provider add`
// called childMap directly instead of the tombstone-aware addLocal helper.
// After a `provider remove`, providers[id] holds the
// {__sennit_tombstone: ...} wrapper; writing flag fields straight into that
// map (rather than into tombstone.Replacement) corrupted the entry so
// ParseTombstone later rejected the whole config. This mirrors
// TestLSPRemoveAbsentAndThenAdd, which already covers `lsp add`.
func TestProviderRemoveThenAdd(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `provider add openai --api-key k1
provider remove openai
provider add openai --api-key k2`)

	entry := result["providers"].(map[string]any)["openai"].(map[string]any)
	tombstone, ok, err := ParseTombstone(entry, "providers", "openai")
	require.NoError(t, err, "provider add after remove must not corrupt the tombstone")
	require.True(t, ok)
	require.Equal(t, "k2", tombstone.Replacement["api_key"])
	require.NotContains(t, tombstone.Replacement, TombstoneKey)
}

// TestProviderRemoveThenModelAdd covers the companion bug in modelAdd: it
// used childMap directly, so `model add` after a `provider remove` (without
// a fresh `provider add`) also corrupted the tombstone instead of routing
// through addLocal.
func TestProviderRemoveThenModelAdd(t *testing.T) {
	t.Parallel()

	result := loadScript(t, `provider add openai --api-key k1
provider remove openai
model add openai/gpt-x --name first`)

	entry := result["providers"].(map[string]any)["openai"].(map[string]any)
	tombstone, ok, err := ParseTombstone(entry, "providers", "openai")
	require.NoError(t, err, "model add after provider remove must not corrupt the tombstone")
	require.True(t, ok)
	models := tombstone.Replacement["models"].([]any)
	require.Len(t, models, 1)
	require.Equal(t, "gpt-x", models[0].(map[string]any)["id"])
}
