package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// codexModelsBody mirrors the shape the Codex backend publishes, trimmed to
// the fields Sennit reads plus one entry of each kind it must drop.
const codexModelsBody = `{
  "models": [
    {
      "slug": "gpt-5.6-sol",
      "display_name": "GPT-5.6-Sol",
      "context_window": 272000,
      "visibility": "list",
      "supported_in_api": true,
      "input_modalities": ["text", "image"],
      "default_reasoning_level": "low",
      "supported_reasoning_levels": [{"effort": "low"}, {"effort": "high"}, {"effort": "max"}]
    },
    {
      "slug": "text-only-model",
      "context_window": 128000,
      "visibility": "list",
      "supported_in_api": true,
      "input_modalities": ["text"]
    },
    {
      "slug": "hidden-model",
      "visibility": "hide",
      "supported_in_api": true
    },
    {
      "slug": "ui-only-model",
      "visibility": "list",
      "supported_in_api": false
    }
  ]
}`

func TestParseModels(t *testing.T) {
	t.Parallel()

	models, err := parseModels([]byte(codexModelsBody))
	require.NoError(t, err)
	require.Len(t, models, 2, "hidden and non-API models must be dropped")

	sol := models[0]
	require.Equal(t, "gpt-5.6-sol", sol.ID)
	require.Equal(t, "GPT-5.6-Sol", sol.Name)
	require.EqualValues(t, 272000, sol.ContextWindow)
	require.Positive(t, sol.DefaultMaxTokens)
	require.Less(t, sol.DefaultMaxTokens, sol.ContextWindow)
	require.True(t, sol.CanReason)
	require.Equal(t, []string{"low", "high", "max"}, sol.ReasoningLevels)
	require.Equal(t, "low", sol.DefaultReasoningEffort)
	require.True(t, sol.SupportsImages)
	require.Zero(t, sol.CostPer1MIn, "a subscription is a flat fee, not per-token")

	plain := models[1]
	require.Equal(t, "text-only-model", plain.Name, "a model with no display name falls back to its slug")
	require.False(t, plain.SupportsImages)
	require.False(t, plain.CanReason)
}

// TestParseModelsAcceptsBareArray: the endpoint has shipped both shapes, and
// either is a valid answer as far as Sennit is concerned.
func TestParseModelsAcceptsBareArray(t *testing.T) {
	t.Parallel()

	models, err := parseModels([]byte(`[{"slug":"m1","supported_in_api":true,"visibility":"list"}]`))
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.EqualValues(t, 272_000, models[0].ContextWindow, "a missing context window falls back rather than landing at zero")
}

// TestParseModelsRejectsEmptyList: an answer that leaves the user with no
// models is an error, not a provider with an empty picker.
func TestParseModelsRejectsEmptyList(t *testing.T) {
	t.Parallel()

	_, err := parseModels([]byte(`{"models":[]}`))
	require.Error(t, err)

	_, err = parseModels([]byte(`not json`))
	require.Error(t, err)
}
