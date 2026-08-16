package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// TestModelMaxOutputTokensPrefersExplicitSetting keeps the ordinary rule
// intact: a per-model setting wins over the catalog default.
func TestModelMaxOutputTokensPrefersExplicitSetting(t *testing.T) {
	t.Parallel()

	model := Model{
		CatalogCfg: catwalk.Model{DefaultMaxTokens: 4096},
		ModelCfg:   config.SelectedModel{Provider: "openai", MaxTokens: 512},
	}
	require.EqualValues(t, 512, modelMaxOutputTokens(model))

	model.ModelCfg.MaxTokens = 0
	require.EqualValues(t, 4096, modelMaxOutputTokens(model))
}

// TestModelMaxOutputTokensZeroForCodex is the regression test for
// "Bad Request: Unsupported parameter: max_output_tokens": the Codex
// endpoint refuses the field, so nothing may put a value on the wire — not
// the catalog, and not a max_tokens the user set by hand.
func TestModelMaxOutputTokensZeroForCodex(t *testing.T) {
	t.Parallel()

	model := Model{
		CatalogCfg: catwalk.Model{DefaultMaxTokens: 4096},
		ModelCfg:   config.SelectedModel{Provider: codex.ProviderID, MaxTokens: 512},
	}
	require.Zero(t, modelMaxOutputTokens(model))
	require.True(t, rejectsMaxOutputTokens(model))

	model.ModelCfg.MaxTokens = 0
	require.Zero(t, modelMaxOutputTokens(model))
}
