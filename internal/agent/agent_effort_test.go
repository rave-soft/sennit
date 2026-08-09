package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/braid/internal/config"
)

// reasoningModel builds a Model whose catwalk config advertises the given
// levels, which is what effectiveReasoningEffort validates against.
func reasoningModel(selected string, levels []string, modelDefault string) Model {
	return Model{
		CatwalkCfg: catwalk.Model{
			CanReason:              true,
			ReasoningLevels:        levels,
			DefaultReasoningEffort: modelDefault,
		},
		ModelCfg: config.SelectedModel{ReasoningEffort: selected},
	}
}

// An agent's reasoning_effort reaches the provider by overriding ModelCfg on
// the agent's own copy of the model, so the behaviour that matters is how
// effectiveReasoningEffort resolves that field.
func TestEffectiveReasoningEffortPrefersAgentOverride(t *testing.T) {
	m := reasoningModel("low", []string{"low", "medium", "high"}, "high")
	require.Equal(t, "low", effectiveReasoningEffort(m))
}

func TestEffectiveReasoningEffortFallsBackWhenLevelUnsupported(t *testing.T) {
	// An agent asking for an effort the model does not offer must not send it
	// through: the model's own default wins instead.
	m := reasoningModel("ultra", []string{"low", "high"}, "high")
	require.Equal(t, "high", effectiveReasoningEffort(m))
}

func TestEffectiveReasoningEffortFallsBackToFirstLevel(t *testing.T) {
	m := reasoningModel("", []string{"medium", "high"}, "")
	require.Equal(t, "medium", effectiveReasoningEffort(m))
}

func TestEffectiveReasoningEffortEmptyForNonReasoningModel(t *testing.T) {
	m := reasoningModel("high", []string{"low", "high"}, "high")
	m.CatwalkCfg.CanReason = false
	require.Empty(t, effectiveReasoningEffort(m))
}

// The override is applied to a copy of the shared Model, so setting it for one
// agent must not leak into the selected-model config other agents read.
func TestAgentEffortOverrideDoesNotMutateShared(t *testing.T) {
	shared := reasoningModel("high", []string{"low", "high"}, "high")

	perAgent := shared
	perAgent.ModelCfg.ReasoningEffort = "low"

	require.Equal(t, "low", effectiveReasoningEffort(perAgent))
	require.Equal(t, "high", effectiveReasoningEffort(shared), "shared model must be untouched")
}

// getProviderOptions is the single place where effort reaches the wire, and
// local servers (llamacpp, ollama, lmstudio) take the default branch. Effort
// used to be dropped there, which made per-agent effort useless for exactly
// the setups that need it most.
func TestGetProviderOptionsSetsEffortForLocalProvider(t *testing.T) {
	m := reasoningModel("low", []string{"low", "medium", "high"}, "high")
	m.CatwalkCfg.ID = "local-model"

	opts := getProviderOptions(m, config.ProviderConfig{Type: "llamacpp"})

	parsed, ok := opts["openai-compat"]
	require.True(t, ok, "local providers are openai-compat under the hood")

	compat, ok := parsed.(*openaicompat.ProviderOptions)
	require.True(t, ok)
	require.NotNil(t, compat.ReasoningEffort, "effort must reach the request")
	require.Equal(t, "low", string(*compat.ReasoningEffort))
}

func TestGetProviderOptionsOmitsUnsupportedEffort(t *testing.T) {
	// The model does not advertise "low", so nothing should be sent rather
	// than a level the server would reject or misread.
	m := reasoningModel("low", []string{"high"}, "")
	m.CatwalkCfg.ID = "local-model"

	opts := getProviderOptions(m, config.ProviderConfig{Type: "llamacpp"})
	compat := opts["openai-compat"].(*openaicompat.ProviderOptions)
	require.NotNil(t, compat.ReasoningEffort)
	require.Equal(t, "high", string(*compat.ReasoningEffort), "falls back to a level the model has")
}
