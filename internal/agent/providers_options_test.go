package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/stretchr/testify/require"
)

// reasoningCatalog builds the catwalk.Model half of getProviderOptions'
// input: everything effectiveReasoningEffort and the CanReason gates read.
func reasoningCatalog(id string, canReason bool, levels []string, defaultEffort string) catwalk.Model {
	return catwalk.Model{ID: id, CanReason: canReason, ReasoningLevels: levels, DefaultReasoningEffort: defaultEffort}
}

func mustOpenAIOptions(t *testing.T, got interface{}) *openai.ProviderOptions {
	t.Helper()
	po, ok := got.(*openai.ProviderOptions)
	require.True(t, ok, "expected *openai.ProviderOptions, got %T", got)
	return po
}

func mustResponsesOptions(t *testing.T, got interface{}) *openai.ResponsesProviderOptions {
	t.Helper()
	po, ok := got.(*openai.ResponsesProviderOptions)
	require.True(t, ok, "expected *openai.ResponsesProviderOptions, got %T", got)
	return po
}

func mustAnthropicOptions(t *testing.T, got interface{}) *anthropic.ProviderOptions {
	t.Helper()
	po, ok := got.(*anthropic.ProviderOptions)
	require.True(t, ok, "expected *anthropic.ProviderOptions, got %T", got)
	return po
}

func mustOpenrouterOptions(t *testing.T, got interface{}) *openrouter.ProviderOptions {
	t.Helper()
	po, ok := got.(*openrouter.ProviderOptions)
	require.True(t, ok, "expected *openrouter.ProviderOptions, got %T", got)
	return po
}

func mustVercelOptions(t *testing.T, got interface{}) *vercel.ProviderOptions {
	t.Helper()
	po, ok := got.(*vercel.ProviderOptions)
	require.True(t, ok, "expected *vercel.ProviderOptions, got %T", got)
	return po
}

func mustGoogleOptions(t *testing.T, got interface{}) *google.ProviderOptions {
	t.Helper()
	po, ok := got.(*google.ProviderOptions)
	require.True(t, ok, "expected *google.ProviderOptions, got %T", got)
	return po
}

func mustCompatOptions(t *testing.T, got interface{}) *openaicompat.ProviderOptions {
	t.Helper()
	po, ok := got.(*openaicompat.ProviderOptions)
	require.True(t, ok, "expected *openaicompat.ProviderOptions, got %T", got)
	return po
}

// TestGetProviderOptionsMergePrecedence pins jsons.Merge's precedence: the
// model-level override wins over the per-provider default, which wins over
// the catalog's own default. A future refactor that reorders the readers
// slice in getProviderOptions would silently invert this.
func TestGetProviderOptionsMergePrecedence(t *testing.T) {
	t.Parallel()

	model := Model{CatalogCfg: reasoningCatalog("custom-model", false, nil, "")}
	model.CatalogCfg.Options.ProviderOptions = map[string]any{"user": "catalog"}
	providerCfg := config.ProviderConfig{Type: "openai"}
	providerCfg.ProviderOptions = map[string]any{"user": "provider-cfg"}
	model.ModelCfg.ProviderOptions = map[string]any{"user": "model-cfg"}

	got := getProviderOptions(model, providerCfg)
	po := mustOpenAIOptions(t, got[openai.Name])
	require.NotNil(t, po.User)
	require.Equal(t, "model-cfg", *po.User, "model.ModelCfg.ProviderOptions must win the three-way merge")

	// Drop the model-level override: the provider config default should win
	// over the catalog default.
	model.ModelCfg.ProviderOptions = nil
	got = getProviderOptions(model, providerCfg)
	po = mustOpenAIOptions(t, got[openai.Name])
	require.Equal(t, "provider-cfg", *po.User)

	// Drop the provider-level override too: only the catalog default is left.
	providerCfg.ProviderOptions = nil
	got = getProviderOptions(model, providerCfg)
	po = mustOpenAIOptions(t, got[openai.Name])
	require.Equal(t, "catalog", *po.User)
}

// TestGetProviderOptionsMalformedInputsDegradeGracefully pins that a source
// that cannot be marshaled, or merged options a provider's ParseOptions
// rejects, degrade to that source being dropped rather than panicking or
// returning an error to the caller.
func TestGetProviderOptionsMalformedInputsDegradeGracefully(t *testing.T) {
	t.Parallel()

	t.Run("unmarshalable model-level options fall back silently", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("custom-model", false, nil, "")}
		// A channel value cannot be JSON-marshaled, so getProviderOptions'
		// json.Marshal(model.ModelCfg.ProviderOptions) fails and the "{}"
		// fallback is used instead of panicking.
		model.ModelCfg.ProviderOptions = map[string]any{"user": make(chan int)}
		providerCfg := config.ProviderConfig{Type: "openai"}
		providerCfg.ProviderOptions = map[string]any{"user": "provider-cfg"}

		require.NotPanics(t, func() {
			got := getProviderOptions(model, providerCfg)
			po := mustOpenAIOptions(t, got[openai.Name])
			require.Equal(t, "provider-cfg", *po.User)
		})
	})

	t.Run("unmarshalable provider-level options fall back silently", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("custom-model", false, nil, "")}
		providerCfg := config.ProviderConfig{Type: "openai"}
		providerCfg.ProviderOptions = map[string]any{"user": make(chan int)}
		model.CatalogCfg.Options.ProviderOptions = map[string]any{"user": "catalog"}

		require.NotPanics(t, func() {
			got := getProviderOptions(model, providerCfg)
			po := mustOpenAIOptions(t, got[openai.Name])
			require.Equal(t, "catalog", *po.User)
		})
	})

	t.Run("unmarshalable catalog-level options fall back silently", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("custom-model", false, nil, "")}
		model.CatalogCfg.Options.ProviderOptions = map[string]any{"user": make(chan int)}
		providerCfg := config.ProviderConfig{Type: "openai"}

		require.NotPanics(t, func() {
			got := getProviderOptions(model, providerCfg)
			po := mustOpenAIOptions(t, got[openai.Name])
			require.Nil(t, po.User)
		})
	})

	t.Run("options a provider rejects are dropped, not a panic", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("claude-x", false, nil, "")}
		providerCfg := config.ProviderConfig{Type: anthropic.Name}
		// "thinking" must unmarshal into a ThinkingProviderOption struct;
		// a bare string fails ParseOptions.
		providerCfg.ProviderOptions = map[string]any{"thinking": "not-an-object"}

		require.NotPanics(t, func() {
			result := getProviderOptions(model, providerCfg)
			require.NotContains(t, result, anthropic.Name, "a provider that rejects the merged options must not appear in the result")
		})
	})
}

// TestGetProviderOptionsOpenAIFamily covers the openai/azure branch,
// including the Responses-API and Codex sub-paths.
func TestGetProviderOptionsOpenAIFamily(t *testing.T) {
	t.Parallel()

	t.Run("chat completions model without reasoning", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("custom-model", false, nil, "")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "openai"})
		po := mustOpenAIOptions(t, got[openai.Name])
		require.Nil(t, po.ReasoningEffort)
	})

	t.Run("chat completions model carries the selected reasoning effort", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("custom-model", true, []string{"low", "medium", "high"}, "low"),
			ModelCfg:   config.SelectedModel{ReasoningEffort: "high"},
		}
		got := getProviderOptions(model, config.ProviderConfig{Type: "azure"})
		po := mustOpenAIOptions(t, got[openai.Name])
		require.NotNil(t, po.ReasoningEffort)
		require.Equal(t, openai.ReasoningEffortHigh, *po.ReasoningEffort)
	})

	t.Run("responses model gets reasoning_summary and include set", func(t *testing.T) {
		t.Parallel()
		// "gpt-5" trips both openai.IsResponsesModel and
		// IsResponsesReasoningModel regardless of CanReason.
		model := Model{CatalogCfg: reasoningCatalog("gpt-5", false, nil, "")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "openai"})
		po := mustResponsesOptions(t, got[openai.Name])
		require.NotNil(t, po.ReasoningSummary)
		require.Equal(t, "auto", *po.ReasoningSummary)
		require.Equal(t, []openai.IncludeType{openai.IncludeReasoningEncryptedContent}, po.Include)
	})

	t.Run("non-responses model without codex ID stays on chat completions", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-local-model", false, nil, "")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "openai", ID: "not-codex"})
		mustOpenAIOptions(t, got[openai.Name])
	})

	t.Run("codex provider ID forces the Responses API for a non-gpt-named model", func(t *testing.T) {
		t.Parallel()
		// A Codex catalog slug the name heuristic would not recognize as a
		// Responses model; the isCodex check must still route it there.
		model := Model{CatalogCfg: reasoningCatalog("codex-auto-review", true, nil, "")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "openai", ID: codex.ProviderID})
		po := mustResponsesOptions(t, got[openai.Name])
		require.NotNil(t, po.ReasoningSummary)
	})

	t.Run("codex provider that cannot reason skips reasoning fields", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("codex-mini", false, nil, "")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "openai", ID: codex.ProviderID})
		po := mustResponsesOptions(t, got[openai.Name])
		require.Nil(t, po.ReasoningSummary)
	})

	t.Run("explicit reasoning_effort in options is not overridden", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("custom-model", true, []string{"low", "high"}, "low"),
		}
		providerCfg := config.ProviderConfig{Type: "openai"}
		providerCfg.ProviderOptions = map[string]any{"reasoning_effort": "high"}
		got := getProviderOptions(model, providerCfg)
		po := mustOpenAIOptions(t, got[openai.Name])
		require.Equal(t, openai.ReasoningEffortHigh, *po.ReasoningEffort)
	})
}

// TestGetProviderOptionsAnthropicFamily covers the anthropic/bedrock branch,
// its default vendor path, and the Alibaba special case.
func TestGetProviderOptionsAnthropicFamily(t *testing.T) {
	t.Parallel()

	t.Run("default vendor sets effort when reasoning is selected", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("claude-x", true, []string{"low", "medium"}, "medium")}
		got := getProviderOptions(model, config.ProviderConfig{Type: anthropic.Name})
		po := mustAnthropicOptions(t, got[anthropic.Name])
		require.NotNil(t, po.Effort)
		require.Equal(t, anthropic.EffortMedium, *po.Effort)
		require.Nil(t, po.Thinking)
	})

	t.Run("default vendor sets a thinking budget when Think is on and no effort applies", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("claude-x", false, nil, ""),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		got := getProviderOptions(model, config.ProviderConfig{Type: "bedrock"})
		po := mustAnthropicOptions(t, got[anthropic.Name])
		require.Nil(t, po.Effort)
		require.NotNil(t, po.Thinking)
		require.Equal(t, int64(2000), po.Thinking.BudgetTokens)
	})

	t.Run("Alibaba routes reasoning effort through extra_body instead of effort", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("qwen-max", true, []string{"low", "medium"}, "medium")}
		providerCfg := config.ProviderConfig{Type: anthropic.Name, ID: string(catwalk.InferenceProviderAlibabaSingapore)}
		got := getProviderOptions(model, providerCfg)
		po := mustAnthropicOptions(t, got[anthropic.Name])
		require.Nil(t, po.Effort, "Alibaba must not use the standard effort field")
	})

	t.Run("Alibaba sets thinking enabled/disabled when no effort applies", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("qwen-max", true, nil, ""),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		providerCfg := config.ProviderConfig{Type: anthropic.Name, ID: string(catwalk.InferenceProviderAlibabaUS)}
		got := getProviderOptions(model, providerCfg)
		po := mustAnthropicOptions(t, got[anthropic.Name])
		require.Nil(t, po.Thinking, "Alibaba routes thinking through extra_body, not the standard field")
		require.Equal(t, map[string]any{"type": "enabled"}, po.ExtraBody["thinking"])
	})
}

// TestGetProviderOptionsOpenrouterAndVercel covers the two branches that
// share the same "reasoning: {enabled, effort}" shape.
func TestGetProviderOptionsOpenrouterAndVercel(t *testing.T) {
	t.Parallel()

	model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low", "medium"}, "medium")}

	t.Run("openrouter", func(t *testing.T) {
		t.Parallel()
		got := getProviderOptions(model, config.ProviderConfig{Type: openrouter.Name})
		po := mustOpenrouterOptions(t, got[openrouter.Name])
		require.NotNil(t, po.Reasoning)
		require.NotNil(t, po.Reasoning.Enabled)
		require.True(t, *po.Reasoning.Enabled)
		require.NotNil(t, po.Reasoning.Effort)
		require.Equal(t, openrouter.ReasoningEffort("medium"), *po.Reasoning.Effort)
	})

	t.Run("vercel", func(t *testing.T) {
		t.Parallel()
		got := getProviderOptions(model, config.ProviderConfig{Type: vercel.Name})
		po := mustVercelOptions(t, got[vercel.Name])
		require.NotNil(t, po.Reasoning)
	})
}

// TestGetProviderOptionsGoogle pins the gemini-2 vs. older-model split, and
// that a thinking_config already present in options is left alone.
func TestGetProviderOptionsGoogle(t *testing.T) {
	t.Parallel()

	t.Run("gemini-2 models get a fixed thinking budget", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("gemini-2.5-pro", true, []string{"low"}, "low")}
		got := getProviderOptions(model, config.ProviderConfig{Type: google.Name})
		po := mustGoogleOptions(t, got[google.Name])
		require.NotNil(t, po.ThinkingConfig)
		require.NotNil(t, po.ThinkingConfig.ThinkingBudget)
		require.Equal(t, int64(2000), *po.ThinkingConfig.ThinkingBudget)
	})

	t.Run("older models get thinking_level from the effective reasoning effort", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("gemini-1.5-pro", true, []string{"low", "high"}, "high")}
		got := getProviderOptions(model, config.ProviderConfig{Type: google.Name})
		po := mustGoogleOptions(t, got[google.Name])
		require.NotNil(t, po.ThinkingConfig)
	})

	t.Run("an explicit thinking_config in options is not overwritten", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("gemini-2.5-pro", true, nil, "")}
		providerCfg := config.ProviderConfig{Type: google.Name}
		providerCfg.ProviderOptions = map[string]any{"thinking_config": map[string]any{"thinking_budget": 42}}
		got := getProviderOptions(model, providerCfg)
		po := mustGoogleOptions(t, got[google.Name])
		require.NotNil(t, po.ThinkingConfig.ThinkingBudget)
		require.Equal(t, int64(42), *po.ThinkingConfig.ThinkingBudget)
	})
}

// TestGetProviderOptionsOpenaicompatVendors covers the openaicompat branch's
// generic path and each per-vendor extra_body special case. One case per
// vendor: the point is that a future refactor cannot silently drop one.
func TestGetProviderOptionsOpenaicompatVendors(t *testing.T) {
	t.Parallel()

	t.Run("generic vendor sets reasoning_effort and an empty extra_body", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low", "medium"}, "medium")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: "some-generic-vendor"}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.NotNil(t, po.ReasoningEffort)
		require.Equal(t, openai.ReasoningEffort("medium"), *po.ReasoningEffort)
	})

	t.Run("IoNet moves the effort into extra_body.reasoning", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low", "medium"}, "medium")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderIoNet)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Nil(t, po.ReasoningEffort, "IoNet must not also set the standard field")
		require.Equal(t, map[string]string{"effort": "medium"}, po.ExtraBody["reasoning"])
	})

	t.Run("IoNet without reasoning selected still flags CanReason models as none", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("some-model", true, nil, ""),
			ModelCfg:   config.SelectedModel{Think: false},
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderIoNet)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]string{"effort": "none"}, po.ExtraBody["reasoning"])
	})

	t.Run("ZAI enables thinking when reasoning is selected", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low"}, "low")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderZAI)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]any{"type": "enabled"}, po.ExtraBody["thinking"])
	})

	t.Run("DeepSeek disables thinking when reasoning is not selected", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", false, nil, "")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderDeepSeek)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]any{"type": "disabled"}, po.ExtraBody["thinking"])
	})

	t.Run("Fireworks skips thinking when reasoning_effort is already set", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("some-model", true, []string{"low"}, "low"),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderFireworks)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.NotContains(t, po.ExtraBody, "thinking", "Fireworks breaks when both reasoning_effort and thinking are set")
	})

	t.Run("Fireworks sets thinking when no reasoning_effort applies", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("some-model", false, nil, ""),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderFireworks)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]any{"type": "enabled"}, po.ExtraBody["thinking"])
	})

	t.Run("Baseten sets chat_template_args.enable_thinking", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("some-model", false, nil, ""),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderBaseten)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]any{"enable_thinking": true}, po.ExtraBody["chat_template_args"])
	})

	t.Run("OpenCodeGo non-MiniMax model uses the standard reasoning_effort field", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low"}, "low")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderOpenCodeGo)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.NotNil(t, po.ReasoningEffort)
		require.NotContains(t, po.ExtraBody, "thinking")
	})

	t.Run("OpenCodeZen MiniMax model uses thinking instead of reasoning_effort", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("MiniMax-M3", true, []string{"low"}, "low"),
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderOpenCodeZen)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Nil(t, po.ReasoningEffort)
		require.Equal(t, map[string]any{"type": "adaptive"}, po.ExtraBody["thinking"])
		require.Equal(t, true, po.ExtraBody["reasoning_split"])
	})

	t.Run("OpenCodeGo MiniMax model disables thinking when reasoning is off", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("minimax-lite", true, nil, "")}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderOpenCodeGo)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, map[string]any{"type": "disabled"}, po.ExtraBody["thinking"])
	})

	t.Run("Alibaba sets enable_thinking under openaicompat too", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("qwen-max", true, nil, ""),
			ModelCfg:   config.SelectedModel{Think: true},
		}
		providerCfg := config.ProviderConfig{Type: openaicompat.Name, ID: string(catwalk.InferenceProviderAlibabaSingapore)}
		got := getProviderOptions(model, providerCfg)
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.Equal(t, true, po.ExtraBody["enable_thinking"])
	})
}

// TestGetProviderOptionsDefaultBranch covers the fallback for provider
// types not in the switch: a registered local-server type (openai-compat
// under the hood) gets reasoning effort carried the same way the
// openaicompat branch does; an unregistered type gets nothing at all.
func TestGetProviderOptionsDefaultBranch(t *testing.T) {
	t.Parallel()

	t.Run("known custom provider carries reasoning effort", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low", "medium"}, "medium")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "ollama"})
		po := mustCompatOptions(t, got[openaicompat.Name])
		require.NotNil(t, po.ReasoningEffort)
	})

	t.Run("unknown provider type yields no options at all", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("some-model", true, []string{"low"}, "low")}
		got := getProviderOptions(model, config.ProviderConfig{Type: "totally-unknown-type"})
		require.Empty(t, got)
	})
}

// TestEffectiveReasoningEffort covers the fallback chain directly: a valid
// user selection wins, then the catalog default, then the first configured
// level, and a model that cannot reason always gets none of it.
func TestEffectiveReasoningEffort(t *testing.T) {
	t.Parallel()

	t.Run("cannot reason yields empty regardless of everything else", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("m", false, []string{"low", "high"}, "high"),
			ModelCfg:   config.SelectedModel{ReasoningEffort: "low"},
		}
		require.Empty(t, effectiveReasoningEffort(model))
	})

	t.Run("valid user selection wins", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("m", true, []string{"low", "medium", "high"}, "medium"),
			ModelCfg:   config.SelectedModel{ReasoningEffort: "high"},
		}
		require.Equal(t, "high", effectiveReasoningEffort(model))
	})

	t.Run("an out-of-range user selection falls back to the catalog default", func(t *testing.T) {
		t.Parallel()
		model := Model{
			CatalogCfg: reasoningCatalog("m", true, []string{"low", "medium"}, "medium"),
			ModelCfg:   config.SelectedModel{ReasoningEffort: "ultra"},
		}
		require.Equal(t, "medium", effectiveReasoningEffort(model))
	})

	t.Run("an out-of-range catalog default falls back to the first configured level", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("m", true, []string{"low", "medium"}, "not-a-level")}
		require.Equal(t, "low", effectiveReasoningEffort(model))
	})

	t.Run("no levels configured at all yields empty", func(t *testing.T) {
		t.Parallel()
		model := Model{CatalogCfg: reasoningCatalog("m", true, nil, "")}
		require.Empty(t, effectiveReasoningEffort(model))
	})
}

// TestIsAnthropicThinking covers coordinator.isAnthropicThinking directly:
// the explicit Think flag, a parsed thinking option, no options at all, and
// options that fail to parse.
func TestIsAnthropicThinking(t *testing.T) {
	t.Parallel()
	c := &coordinator{}

	t.Run("explicit Think flag wins outright", func(t *testing.T) {
		t.Parallel()
		require.True(t, c.isAnthropicThinking(config.SelectedModel{Think: true}))
	})

	t.Run("no provider options means no thinking", func(t *testing.T) {
		t.Parallel()
		require.False(t, c.isAnthropicThinking(config.SelectedModel{}))
	})

	t.Run("a parsed thinking option is detected", func(t *testing.T) {
		t.Parallel()
		model := config.SelectedModel{ProviderOptions: map[string]any{
			"thinking": map[string]any{"budget_tokens": 2000},
		}}
		require.True(t, c.isAnthropicThinking(model))
	})

	t.Run("options that fail to parse are treated as no thinking", func(t *testing.T) {
		t.Parallel()
		model := config.SelectedModel{ProviderOptions: map[string]any{"thinking": "not-an-object"}}
		require.False(t, c.isAnthropicThinking(model))
	})
}
