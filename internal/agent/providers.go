package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"

	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/openai/openai-go/v3/option"
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
		return options
	}

	mergedOptions := make(map[string]any)

	err = json.Unmarshal(got, &mergedOptions)
	if err != nil {
		slog.Error("Could not create config for call", "err", err)
		return options
	}

	reasoningEffort := effectiveReasoningEffort(model)
	shouldSetEffort := model.CatalogCfg.CanReason &&
		reasoningEffort != "" &&
		slices.Contains(model.CatalogCfg.ReasoningLevels, reasoningEffort)

	switch providerCfg.Type {
	case openai.Name, azure.Name:
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

	case anthropic.Name, bedrock.Name:
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

	case openrouter.Name:
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

	case vercel.Name:
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

	case google.Name:
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

	case openaicompat.Name:
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

		// "reasoning effort" is a standard OpenAI field, but "thinking" is not.
		// Setting it in the right way for each provider.
		// TODO: Abstract this in Fantasy somehow?
		// TODO: Allow custom providers to specify how to set this?
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

	default:
		// Known custom providers (litellm, ollama, omlx, llamacpp, lmstudio)
		// are openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			// Carry reasoning effort the same way the openai-compat branch
			// does. Without this an agent's reasoning_effort is silently
			// dropped for every local server, which is exactly where
			// per-agent effort is most useful. Servers that do not know the
			// field ignore it, so sending it costs nothing; it is still gated
			// on the model advertising the level.
			if _, has := mergedOptions["reasoning_effort"]; !has && shouldSetEffort {
				mergedOptions["reasoning_effort"] = reasoningEffort
			}
			parsed, err := openaicompat.ParseOptions(mergedOptions)
			if err == nil {
				options[openaicompat.Name] = parsed
			}
		}
	}

	return options
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

// buildProviderHTTPClient returns an *http.Client composing proxy routing
// (proxyURL) with debug request logging, or (nil, nil) if neither applies —
// callers should skip WithHTTPClient in that case and use the SDK's default
// client. Proxying and debug logging compose: when both are set, requests
// go through the proxy and are logged.
func (c *coordinator) buildProviderHTTPClient(proxyURL string) (*http.Client, error) {
	debug := c.cfg.Config().Options.Debug
	if proxyURL == "" && !debug {
		return nil, nil
	}
	transport, err := buildProxyTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	if debug {
		transport = &log.HTTPRoundTripLogger{Transport: transport}
	}
	return &http.Client{Transport: transport}, nil
}

func (c *coordinator) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string) (fantasy.Provider, error) {
	var opts []anthropic.Option
	authIsBearer := false

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		headers["Authorization"] = apiKey
		authIsBearer = true
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		headers["Authorization"] = "Bearer " + apiKey
		authIsBearer = true
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	httpClient, err := c.buildProviderHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	if authIsBearer {
		// Auth goes through Authorization above, so we never pass
		// anthropic.WithAPIKey — which means the SDK's own
		// DefaultClientOptions falls back to reading $ANTHROPIC_API_KEY and
		// setting X-Api-Key from it, duplicating (or contradicting) the
		// Bearer token. option.WithAPIKey("") is not a fix: WithHeader uses
		// Header.Set, so it would send an empty X-Api-Key rather than omit
		// it. This used to be worked around with
		// os.Setenv("ANTHROPIC_API_KEY", ""), which corrupted the key for
		// every other provider built afterwards and every subprocess
		// Sennit spawns. Stripping the header at the transport, the same
		// seam azureAPIVersionTransport uses below, is local and leaves
		// the environment untouched.
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = &stripHeaderTransport{
			base:   httpClient.Transport,
			header: "X-Api-Key",
		}
	}
	if httpClient != nil {
		opts = append(opts, anthropic.WithHTTPClient(httpClient))
	}
	return anthropic.New(opts...)
}

// stripHeaderTransport deletes a header the SDK set from its own defaults
// (see buildAnthropicProvider) before the request goes out, without
// touching the process environment those defaults were read from.
type stripHeaderTransport struct {
	base   http.RoundTripper
	header string
}

func (t *stripHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Header.Get(t.header) != "" {
		req = req.Clone(req.Context())
		req.Header.Del(t.header)
	}
	return base.RoundTrip(req)
}

func (c *coordinator) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	httpClient, err := c.buildProviderHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	if providerID == codex.ProviderID {
		// Codex quotes the account's plan and remaining allowance on every
		// response, so the sidebar's figures come from ordinary traffic
		// rather than a separate poll — but only if something reads the
		// headers, which is what this transport is for.
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = codex.NewUsageTransport(httpClient.Transport)
	}
	if httpClient != nil {
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (c *coordinator) buildOpenrouterProvider(_, apiKey string, headers map[string]string, proxyURL string) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if httpClient, err := c.buildProviderHTTPClient(proxyURL); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (c *coordinator) buildVercelProvider(_, apiKey string, headers map[string]string, proxyURL string) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if httpClient, err := c.buildProviderHTTPClient(proxyURL); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (c *coordinator) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, proxyURL string) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		proxyTransport, err := buildProxyTransport(proxyURL)
		if err != nil {
			return nil, err
		}
		httpClient = copilot.NewClient(isSubAgent, c.cfg.Config().Options.Debug, proxyTransport)
	}
	if httpClient == nil {
		var err error
		httpClient, err = c.buildProviderHTTPClient(proxyURL)
		if err != nil {
			return nil, err
		}
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (c *coordinator) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, proxyURL string) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	httpClient, err := c.buildProviderHTTPClient(proxyURL)
	if err != nil {
		return nil, err
	}
	if options == nil {
		options = make(map[string]string)
	}
	// fantasy's azure provider (charm.land/fantasy/providers/azure) stores
	// WithAPIVersion but never reads it back out (confirmed against our
	// pinned v0.40.0 and the newest published v0.41.2 alike) — azure.New
	// never applies it to the request, so passing it straight through
	// would silently do nothing. fantasy does let us supply the HTTP
	// client every Azure request goes through (WithHTTPClient, same seam
	// codex.NewUsageTransport above uses), so we honour the setting
	// ourselves by adding the api-version query parameter at the
	// transport level instead of dropping it.
	if apiVersion := options["apiVersion"]; apiVersion != "" {
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = &azureAPIVersionTransport{
			base:       httpClient.Transport,
			apiVersion: apiVersion,
		}
	}
	if httpClient != nil {
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

// azureAPIVersionTransport adds Azure's required "api-version" query
// parameter to every outgoing request, since fantasy's azure provider
// accepts the option but never applies it (see buildAzureProvider). It
// leaves an already-present api-version alone, in case a future fantasy
// release starts setting one itself.
type azureAPIVersionTransport struct {
	base       http.RoundTripper
	apiVersion string
}

func (t *azureAPIVersionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.URL != nil && req.URL.Query().Get("api-version") == "" {
		req = req.Clone(req.Context())
		q := req.URL.Query()
		q.Set("api-version", t.apiVersion)
		req.URL.RawQuery = q.Encode()
	}
	return base.RoundTrip(req)
}

func (c *coordinator) buildBedrockProvider(apiKey string, headers map[string]string, providerID, proxyURL string) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if httpClient, err := c.buildProviderHTTPClient(proxyURL); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		opts = append(opts, bedrock.WithRegion("us-east-1"))
	}

	return bedrock.New(opts...)
}

func (c *coordinator) buildGoogleProvider(baseURL, apiKey string, headers map[string]string, proxyURL string) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if httpClient, err := c.buildProviderHTTPClient(proxyURL); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (c *coordinator) buildGoogleVertexProvider(headers map[string]string, options map[string]string, proxyURL string) (fantasy.Provider, error) {
	opts := []google.Option{}
	if httpClient, err := c.buildProviderHTTPClient(proxyURL); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (c *coordinator) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (c *coordinator) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && c.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	apiKey, _ := c.cfg.Resolve(providerCfg.APIKey)
	baseURL, _ := c.cfg.Resolve(providerCfg.BaseURL)

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL)
		}
	}

	switch providerCfg.Type {
	case openai.Name:
		return c.buildOpenaiProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL)
	case anthropic.Name:
		return c.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL)
	case openrouter.Name:
		return c.buildOpenrouterProvider(baseURL, apiKey, headers, providerCfg.ProxyURL)
	case vercel.Name:
		return c.buildVercelProvider(baseURL, apiKey, headers, providerCfg.ProxyURL)
	case azure.Name:
		return c.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams, providerCfg.ProxyURL)
	case bedrock.Name:
		return c.buildBedrockProvider(apiKey, headers, providerCfg.ID, providerCfg.ProxyURL)
	case google.Name:
		return c.buildGoogleProvider(baseURL, apiKey, headers, providerCfg.ProxyURL)
	case "google-vertex":
		return c.buildGoogleVertexProvider(headers, providerCfg.ExtraParams, providerCfg.ProxyURL)
	case openaicompat.Name:
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderZAI):
			// Clone before writing: providerCfg.ExtraBody is shared with
			// the stored *config.Config, and mutating it in place would
			// race other readers and leak the flag into later generations.
			extraBody := maps.Clone(providerCfg.ExtraBody)
			if extraBody == nil {
				extraBody = map[string]any{}
			}
			extraBody["tool_stream"] = true
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, extraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL)
		}
		return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL)
	default:
		// Known custom providers (litellm, ollama, omlx) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return c.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}
