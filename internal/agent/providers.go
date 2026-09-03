package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/providers/typeclass"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/qjebbs/go-jsons"
)

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
	"gpt-5.6-luna":  true,
	"gpt-5.6-terra": true,
	"gpt-5.6-sol":   true,
}

// OpenCode models that user Anthropic Messages API instead of Chat Completions.
var opencodeMessagesModels = map[string]bool{
	"qwen3.7-max": true,
}

// effectiveReasoningEffort returns the reasoning effort to apply for provider calls.
// It prefers the user-selected effort when valid, otherwise the model default when
// valid, and finally falls back to the first configured reasoning level.
func effectiveReasoningEffort(model Model) string {
	if !model.CatalogCfg.CanReason {
		return ""
	}

	if effort := model.ModelCfg.ReasoningEffort; effort != "" && slices.Contains(model.CatalogCfg.ReasoningLevels, effort) {
		return effort
	}
	if effort := model.CatalogCfg.DefaultReasoningEffort; effort != "" && slices.Contains(model.CatalogCfg.ReasoningLevels, effort) {
		return effort
	}
	if len(model.CatalogCfg.ReasoningLevels) > 0 {
		return model.CatalogCfg.ReasoningLevels[0]
	}
	return ""
}

func getProviderOptions(model Model, providerCfg config.ProviderConfig) fantasy.ProviderOptions {
	options := fantasy.ProviderOptions{}

	mergedOptions, err := mergeProviderOptions(model, providerCfg)
	if err != nil {
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatalogCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatalogCfg.ReasoningLevels, reasoningEffort)

	switch typeclass.Of(providerCfg.Type) {
	case typeclass.OpenAI, typeclass.Azure:
		setOpenAIProviderOptions(options, model, providerCfg, mergedOptions, reasoningEffort, shouldSetEffort)
	case typeclass.Anthropic, typeclass.Bedrock:
		setAnthropicProviderOptions(options, model, providerCfg, mergedOptions, reasoningEffort, shouldSetEffort)
	case typeclass.OpenRouter:
		setOpenRouterProviderOptions(options, mergedOptions, reasoningEffort, shouldSetEffort)
	case typeclass.Vercel:
		setVercelProviderOptions(options, mergedOptions, reasoningEffort, shouldSetEffort)
	case typeclass.Google:
		setGoogleProviderOptions(options, model, mergedOptions, reasoningEffort)
	case typeclass.OpenAICompat:
		setOpenAICompatProviderOptions(options, model, providerCfg, mergedOptions, reasoningEffort, shouldSetEffort)
	default:
		setCustomProviderOptions(options, providerCfg, mergedOptions, reasoningEffort, shouldSetEffort)
	}

	return options
}

// mergeProviderOptions layers the model's own provider-options config over
// the provider's, over the catalog's defaults (highest precedence first:
// model config, then provider config, then catalog), and decodes the
// result into a plain map for the per-provider option builders below.
func mergeProviderOptions(model Model, providerCfg config.ProviderConfig) (map[string]any, error) {
	cfgOpts := []byte("{}")
	providerCfgOpts := []byte("{}")
	catwalkOpts := []byte("{}")

	if model.ModelCfg.ProviderOptions != nil {
		data, err := json.Marshal(model.ModelCfg.ProviderOptions)
		if err == nil {
			cfgOpts = data
		}
	}

	if providerCfg.ProviderOptions != nil {
		data, err := json.Marshal(providerCfg.ProviderOptions)
		if err == nil {
			providerCfgOpts = data
		}
	}

	if model.CatalogCfg.Options.ProviderOptions != nil {
		data, err := json.Marshal(model.CatalogCfg.Options.ProviderOptions)
		if err == nil {
			catwalkOpts = data
		}
	}

	readers := []io.Reader{
		bytes.NewReader(catwalkOpts),
		bytes.NewReader(providerCfgOpts),
		bytes.NewReader(cfgOpts),
	}

	got, err := jsons.Merge(readers)
	if err != nil {
		slog.Error("Could not merge call config", "err", err)
		return nil, err
	}

	mergedOptions := make(map[string]any)
	if err := json.Unmarshal(got, &mergedOptions); err != nil {
		slog.Error("Could not create config for call", "err", err)
		return nil, err
	}
	return mergedOptions, nil
}

// setOpenAIProviderOptions fills in the reasoning fields OpenAI/Azure
// models expect and parses mergedOptions into options, choosing the
// Responses API shape for Codex and Responses-only models (Chat
// Completions otherwise).
func setOpenAIProviderOptions(options fantasy.ProviderOptions, model Model, providerCfg config.ProviderConfig, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	_, hasReasoningEffort := mergedOptions["reasoning_effort"]
	if !hasReasoningEffort && shouldSetEffort {
		mergedOptions["reasoning_effort"] = reasoningEffort
	}
	// Codex only speaks the Responses API, whatever a model is named:
	// its catalog carries slugs like "codex-auto-review" that the
	// name-based heuristic below does not recognize, and treating one
	// of those as a Chat Completions model would build options for an
	// API this provider never calls.
	isCodex := providerCfg.ID == codex.ProviderID
	if isCodex || openai.IsResponsesModel(model.CatalogCfg.ID) {
		if (isCodex && model.CatalogCfg.CanReason) || openai.IsResponsesReasoningModel(model.CatalogCfg.ID) {
			mergedOptions["reasoning_summary"] = "auto"
			mergedOptions["include"] = []openai.IncludeType{openai.IncludeReasoningEncryptedContent}
		}
		parsed, err := openai.ParseResponsesOptions(mergedOptions)
		if err == nil {
			options[openai.Name] = parsed
		}
	} else {
		parsed, err := openai.ParseOptions(mergedOptions)
		if err == nil {
			options[openai.Name] = parsed
		}
	}
}

// setAnthropicProviderOptions fills in the reasoning fields Anthropic and
// Bedrock models expect - "effort"/"thinking" for most providers, an
// Alibaba-specific extra_body shape for the Alibaba inference providers -
// and parses mergedOptions into options.
func setAnthropicProviderOptions(options fantasy.ProviderOptions, model Model, providerCfg config.ProviderConfig, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	var (
		_, hasEffort = mergedOptions["effort"]
		_, hasThink  = mergedOptions["thinking"]
		extraBody    = make(map[string]any)
	)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
		switch {
		case !hasEffort && shouldSetEffort:
			extraBody["reasoning_effort"] = reasoningEffort
		case !hasThink && model.CatalogCfg.CanReason:
			if model.ModelCfg.Think {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}
		mergedOptions["extra_body"] = extraBody

	default:
		switch {
		case !hasEffort && shouldSetEffort:
			mergedOptions["effort"] = reasoningEffort
		case !hasThink && model.ModelCfg.Think:
			mergedOptions["thinking"] = map[string]any{"budget_tokens": 2000}
		}
	}

	parsed, err := anthropic.ParseOptions(mergedOptions)
	if err == nil {
		options[anthropic.Name] = parsed
	}
}

// setOpenRouterProviderOptions fills in the "reasoning" field OpenRouter
// expects and parses mergedOptions into options.
func setOpenRouterProviderOptions(options fantasy.ProviderOptions, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	_, hasReasoning := mergedOptions["reasoning"]
	if !hasReasoning && shouldSetEffort {
		mergedOptions["reasoning"] = map[string]any{
			"enabled": true,
			"effort":  reasoningEffort,
		}
	}
	parsed, err := openrouter.ParseOptions(mergedOptions)
	if err == nil {
		options[openrouter.Name] = parsed
	}
}

// setVercelProviderOptions fills in the "reasoning" field Vercel expects
// and parses mergedOptions into options.
func setVercelProviderOptions(options fantasy.ProviderOptions, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	_, hasReasoning := mergedOptions["reasoning"]
	if !hasReasoning && shouldSetEffort {
		mergedOptions["reasoning"] = map[string]any{
			"enabled": true,
			"effort":  reasoningEffort,
		}
	}
	parsed, err := vercel.ParseOptions(mergedOptions)
	if err == nil {
		options[vercel.Name] = parsed
	}
}

// setGoogleProviderOptions fills in the "thinking_config" field Google
// expects and parses mergedOptions into options.
func setGoogleProviderOptions(options fantasy.ProviderOptions, model Model, mergedOptions map[string]any, reasoningEffort string) {
	_, hasReasoning := mergedOptions["thinking_config"]
	if !hasReasoning && model.CatalogCfg.CanReason {
		if strings.HasPrefix(model.CatalogCfg.ID, "gemini-2") {
			mergedOptions["thinking_config"] = map[string]any{
				"thinking_budget":  2000,
				"include_thoughts": true,
			}
		} else {
			mergedOptions["thinking_config"] = map[string]any{
				"thinking_level":   reasoningEffort,
				"include_thoughts": true,
			}
		}
	}
	parsed, err := google.ParseOptions(mergedOptions)
	if err == nil {
		options[google.Name] = parsed
	}
}

// setOpenAICompatProviderOptions fills in the provider-specific reasoning
// fields the OpenAI-compat providers expect - "reasoning_effort" is a
// standard OpenAI field, but "thinking" is not, so each provider that uses
// it needs its own extra_body shape - and parses mergedOptions into
// options.
//
// TODO: Abstract this in Fantasy somehow?
// TODO: Allow custom providers to specify how to set this?
func setOpenAICompatProviderOptions(options fantasy.ProviderOptions, model Model, providerCfg config.ProviderConfig, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	extraBody := make(map[string]any)

	_, hasReasoningEffort := mergedOptions["reasoning_effort"]
	if !hasReasoningEffort && shouldSetEffort {
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderIoNet):
			extraBody["reasoning"] = map[string]string{"effort": reasoningEffort}
		case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
			// MiniMax models use the "thinking" parameter instead of
			// "reasoning_effort". Other models on these providers still
			// use the standard field.
			if !strings.HasPrefix(strings.ToLower(model.CatalogCfg.ID), "minimax") {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
		default:
			mergedOptions["reasoning_effort"] = reasoningEffort
		}
	}

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderIoNet):
		if _, ok := extraBody["reasoning"]; !ok && model.CatalogCfg.CanReason {
			if model.ModelCfg.Think {
				extraBody["reasoning"] = map[string]string{"effort": "medium"}
			} else {
				extraBody["reasoning"] = map[string]string{"effort": "none"}
			}
		}

	case string(catwalk.InferenceProviderZAI), string(catwalk.InferenceProviderDeepSeek):
		if model.ModelCfg.Think || reasoningEffort != "" {
			extraBody["thinking"] = map[string]any{"type": "enabled"}
		} else {
			extraBody["thinking"] = map[string]any{"type": "disabled"}
		}

	case string(catwalk.InferenceProviderFireworks):
		// NOTE: Fireworks break if we set both `reasoning_effort` and `thinking`.
		if reasoningEffort == "" {
			if model.ModelCfg.Think {
				extraBody["thinking"] = map[string]any{"type": "enabled"}
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}

	case string(catwalk.InferenceProviderBaseten):
		extraBody["chat_template_args"] = map[string]any{
			"enable_thinking": model.ModelCfg.Think || reasoningEffort != "" && reasoningEffort != "none",
		}

	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		// MiniMax M3 uses the "thinking" parameter to control reasoning.
		// "reasoning_split" must be true so thinking content is returned
		// in the "reasoning_content" field instead of inline in "content".
		if strings.HasPrefix(strings.ToLower(model.CatalogCfg.ID), "minimax") {
			if model.CatalogCfg.CanReason && (model.ModelCfg.Think || reasoningEffort != "") {
				extraBody["thinking"] = map[string]any{"type": "adaptive"}
				extraBody["reasoning_split"] = true
			} else {
				extraBody["thinking"] = map[string]any{"type": "disabled"}
			}
		}

	case string(catwalk.InferenceProviderAlibabaSingapore), string(catwalk.InferenceProviderAlibabaUS):
		if model.CatalogCfg.CanReason {
			extraBody["enable_thinking"] = model.ModelCfg.Think || reasoningEffort != ""
		}
	}

	mergedOptions["extra_body"] = extraBody

	parsed, err := openaicompat.ParseOptions(mergedOptions)
	if err == nil {
		options[openaicompat.Name] = parsed
	}
}

// setCustomProviderOptions handles a provider type typeclass does not
// recognize: known custom providers (litellm, ollama, omlx, llamacpp,
// lmstudio) are openai-compat under the hood, so reasoning effort is
// carried the same way the openai-compat branch does. Without this an
// agent's reasoning_effort is silently dropped for every local server,
// which is exactly where per-agent effort is most useful. Servers that do
// not know the field ignore it, so sending it costs nothing; it is still
// gated on the model advertising the level. An unrecognized provider type
// gets no options at all.
func setCustomProviderOptions(options fantasy.ProviderOptions, providerCfg config.ProviderConfig, mergedOptions map[string]any, reasoningEffort string, shouldSetEffort bool) {
	if !discover.IsKnownCustomProvider(string(providerCfg.Type)) {
		return
	}
	if _, has := mergedOptions["reasoning_effort"]; !has && shouldSetEffort {
		mergedOptions["reasoning_effort"] = reasoningEffort
	}
	parsed, err := openaicompat.ParseOptions(mergedOptions)
	if err == nil {
		options[openaicompat.Name] = parsed
	}
}

// buildProxyTransport returns an http.RoundTripper that routes requests
// through proxyURL, or nil if proxyURL is empty (callers fall back to
// http.DefaultTransport).
func buildProxyTransport(proxyURL string) (http.RoundTripper, error) {
	if proxyURL == "" {
		return nil, nil
	}
	proxyClient, err := config.NewProxyHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	return proxyClient.Transport, nil
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, model Model) []fantasy.Message {
	providerSupportsMedia := model.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrockEurope)

	if providerSupportsMedia {
		return messages
	}

	supportsImages := model.CatalogCfg.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	return cacheControlOptions()
}

func cacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv(brand.EnvPrefix + "DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		bedrock.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		vercel.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
}
