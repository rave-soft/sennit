package agent

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/openai/openai-go/v3/option"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/providers/typeclass"
)

// buildProviderHTTPClient returns an *http.Client composing proxy routing
// (proxyURL) with debug request logging, or (nil, nil) if neither applies —
// callers should skip WithHTTPClient in that case and use the SDK's default
// client. Proxying and debug logging compose: when both are set, requests
// go through the proxy and are logged.
func (b *runtimeBuilder) buildProviderHTTPClient(proxyURL string) (*http.Client, error) {
	return buildProviderHTTPClient(proxyURL, b.cfg.Config().Options.Debug)
}

func buildProviderHTTPClient(proxyURL string, debug bool) (*http.Client, error) {
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

func (b *runtimeBuilder) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
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

	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
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
		// it. Stripping the header at the transport, the same seam
		// azureAPIVersionTransport uses in providers.go, is local and
		// leaves the environment untouched — unlike setting
		// $ANTHROPIC_API_KEY to "", which would corrupt the key for every
		// other provider built afterwards and every subprocess Sennit
		// spawns.
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

func (b *runtimeBuilder) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
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

func (b *runtimeBuilder) buildOpenrouterProvider(_, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (b *runtimeBuilder) buildVercelProvider(_, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (b *runtimeBuilder) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, proxyURL string, debug bool) (fantasy.Provider, error) {
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
		httpClient = copilot.NewClient(isSubAgent, debug, proxyTransport)
	}
	if httpClient == nil {
		var err error
		httpClient, err = buildProviderHTTPClient(proxyURL, debug)
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

func (b *runtimeBuilder) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
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

func (b *runtimeBuilder) buildBedrockProvider(apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	usingAPIKeyAuth := false
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
		usingAPIKeyAuth = true
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
		usingAPIKeyAuth = true
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		// The EU variant always forces its region: it exists specifically
		// to pin requests to eu-west-1, so there is no "let the user's own
		// region win" case here.
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		// Only force us-east-1 as a last resort, and only where it can
		// actually change anything. fantasy's anthropic provider
		// (anthropic.go:289-296) consults the region we pass here in two
		// different ways depending on auth: with an API key or skipAuth it
		// builds config via bedrockBasicAuthConfig, which already does
		// cmp.Or(region, "us-east-1") and never looks at the environment -
		// so our forcing it here changes nothing and the check below would
		// be wasted work. Only the SDK-auth path (no API key) does
		// cfg.Region = cmp.Or(bedrockRegion, cfg.Region) against a
		// LoadDefaultConfig result, where any region we pass always wins
		// over one the environment already supplies.
		//
		// We don't call LoadDefaultConfig ourselves to check: on a
		// non-EC2 box with no AWS config, region resolution can fall
		// through to the EC2 instance-metadata endpoint and stall a
		// provider build (this runs on every config reload and model
		// switch, not just once). AWS_REGION/AWS_DEFAULT_REGION is a
		// cheaper, synchronous proxy for "the environment supplies a
		// region" - it won't see a region set only in a shared AWS config
		// profile, so a profile-only region still gets overridden to
		// us-east-1, but it covers the common case without risking a
		// network stall in the UI's path.
		if !usingAPIKeyAuth && os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			opts = append(opts, bedrock.WithRegion("us-east-1"))
		}
	}

	return bedrock.New(opts...)
}

func (b *runtimeBuilder) buildGoogleProvider(baseURL, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (b *runtimeBuilder) buildGoogleVertexProvider(headers map[string]string, options map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []google.Option{}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
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

func (b *runtimeBuilder) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

// buildProviderForSnapshot returns a fantasy.Provider for the configured
// provider, resolving its API key and base URL through the config's
// shell-expansion resolver.
func (b *runtimeBuilder) buildProviderForSnapshot(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, snapshot runtimeConfigSnapshot) (fantasy.Provider, error) {
	effective, ok := snapshot.config.RuntimeProvider(providerCfg.ID)
	if !ok {
		return nil, errModelProviderNotConfigured
	}
	headers := maps.Clone(effective.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if typeclass.Of(providerCfg.Type) == typeclass.Anthropic && b.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	// A resolve failure (an env var that is not set, a shell command that
	// exits non-zero) leaves the value empty, and the provider then fails
	// on an empty key or URL with nothing pointing at the cause. Log it
	// here rather than dropping it: the call still proceeds on what it
	// has, but the reason is now on the record.
	apiKey := effective.APIKey
	baseURL := effective.BaseURL

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return b.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, effective.ProxyURL, snapshot.config.Options.Debug)
		}
	}

	switch typeclass.Of(providerCfg.Type) {
	case typeclass.OpenAI:
		return b.buildOpenaiProvider(baseURL, apiKey, headers, providerCfg.ID, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.Anthropic:
		return b.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.OpenRouter:
		return b.buildOpenrouterProvider(baseURL, apiKey, headers, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.Vercel:
		return b.buildVercelProvider(baseURL, apiKey, headers, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.Azure:
		return b.buildAzureProvider(baseURL, apiKey, headers, effective.ExtraParams, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.Bedrock:
		return b.buildBedrockProvider(apiKey, headers, providerCfg.ID, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.Google:
		return b.buildGoogleProvider(baseURL, apiKey, headers, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.GoogleVertex:
		return b.buildGoogleVertexProvider(headers, effective.ExtraParams, effective.ProxyURL, snapshot.config.Options.Debug)
	case typeclass.OpenAICompat:
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
			return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, extraBody, providerCfg.ID, isSubAgent, effective.ProxyURL, snapshot.config.Options.Debug)
		}
		return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, effective.ProxyURL, snapshot.config.Options.Debug)
	default:
		// Known custom providers (litellm, ollama, omlx) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, effective.ProxyURL, snapshot.config.Options.Debug)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}
