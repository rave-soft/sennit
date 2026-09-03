package config_test

import (
	"slices"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

// TestShellConfigProviderAddAndModel covers the provider/model catalog
// builtins plus model selection via the single-model `model <provider/id>`
// form (internal/shellconfig has no more large/small slots).
func TestShellConfigProviderAddAndModel(t *testing.T) {
	store := loadSennitShGlobal(t, `provider add myllm \
  --type openai-compat \
  --base-url "http://localhost:1234/v1" \
  --api-key "sk-test" \
  --discover-models false \
  --extra-body '{"service_tier":"flex"}' \
  --provider-options '{"region":"local"}'
provider add myllm \
  --extra-body '{"stream":true}' \
  --provider-options '{"mode":"test"}'
model add myllm/foo-1 --name "Foo 1" --context-window 8000 \
  --price-input 1.25 --price-output 5 \
  --price-cache-create 2 --price-cache-hit 0.25
model myllm/foo-1 --think`)

	cfg := store.Config()

	p, ok := cfg.Providers.Get("myllm")
	require.True(t, ok, "myllm provider should be configured")
	require.Equal(t, "sk-test", p.APIKey)
	require.Equal(t, "http://localhost:1234/v1", p.BaseURL)
	require.NotNil(t, p.AutoDiscoverModels)
	require.False(t, *p.AutoDiscoverModels)
	require.Equal(t, "flex", p.ExtraBody["service_tier"])
	require.Equal(t, true, p.ExtraBody["stream"])
	require.Equal(t, "local", p.ProviderOptions["region"])
	require.Equal(t, "test", p.ProviderOptions["mode"])
	require.True(
		t,
		slices.ContainsFunc(p.Models, func(m catwalk.Model) bool { return m.ID == "foo-1" }),
		"custom model foo-1 should be in the provider catalog",
	)
	model := p.Models[0]
	require.Equal(t, 1.25, model.CostPer1MIn)
	require.Equal(t, 5.0, model.CostPer1MOut)
	require.Equal(t, 2.0, model.CostPer1MInCached)
	require.Equal(t, 0.25, model.CostPer1MOutCached)

	require.Equal(t, "myllm", store.Config().Model.Provider)
	require.Equal(t, "foo-1", store.Config().Model.Model)
	require.True(t, store.Config().Model.Think)
}

func TestShellConfigProviderRemove(t *testing.T) {
	// Both providers get a model so they survive provider configuration
	// (model-less providers are dropped); the only difference is the remove.
	store := loadSennitShGlobal(t, `provider add keepme --type openai-compat --base-url "http://localhost:1/v1" --api-key k
model add keepme/m1 --name M1
provider add dropme --type openai-compat --base-url "http://localhost:2/v1" --api-key k
model add dropme/m2 --name M2
provider remove dropme`)

	_, keep := store.Config().Providers.Get("keepme")
	_, drop := store.Config().Providers.Get("dropme")
	require.True(t, keep, "keepme should remain")
	require.False(t, drop, "dropme should be gone after remove")
}

// TestShellConfigModelRemoveAfterProviderRemove is the regression test for
// `model remove` writing into a provider's tombstone: `provider remove`
// leaves a {__sennit_tombstone: ...} wrapper in providers[id], and a naive
// `model remove` on that same id used to add a "models" key beside the
// marker, which ParseTombstone rejects — failing the whole config load
// with a tombstone error unrelated to what the script actually said. The
// provider stays removed; `model remove` on an already-removed provider is
// a no-op, matching `model remove` on a provider that was never declared.
// Loading through the real pipeline (not just the builder's in-memory
// shape) is what would have caught the original bug: the corruption only
// surfaces when applyLayerTombstones parses the merged JSON.
func TestShellConfigModelRemoveAfterProviderRemove(t *testing.T) {
	store, err := loadSennitShGlobalErr(t, `provider add dropme --type openai-compat --base-url "http://localhost:2/v1" --api-key k
model add dropme/m2 --name M2
provider remove dropme
model remove dropme/m2`)
	require.NoError(t, err, "removing a model from an already-removed provider must not corrupt the config")

	_, drop := store.Config().Providers.Get("dropme")
	require.False(t, drop, "dropme must stay removed, not be resurrected by the model remove")
}

// TestShellConfigModelRemoveBeforeProviderRemove pins the reverse order,
// documented as harmless: removing a model while the provider still has its
// real entry, then removing the provider.
func TestShellConfigModelRemoveBeforeProviderRemove(t *testing.T) {
	store := loadSennitShGlobal(t, `provider add dropme --type openai-compat --base-url "http://localhost:2/v1" --api-key k
model add dropme/m2 --name M2
model remove dropme/m2
provider remove dropme`)

	_, drop := store.Config().Providers.Get("dropme")
	require.False(t, drop, "dropme should be gone after remove")
}
