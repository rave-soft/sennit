package config

// This file holds the provider entry as it appears in the config file
// (ProviderConfig, the fields sennitrc/sennit.json can set for a provider)
// plus the per-vendor setup that turns one into something usable: filling in
// the headers GitHub Copilot and Codex expect, converting to catwalk's
// provider shape, and probing a provider's credentials over the network.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/providers/accounts"
)

type ProviderConfig struct {
	// The provider's id.
	ID string `json:"id,omitempty" jsonschema:"description=Unique identifier for the provider,example=openai"`
	// The provider's name, used for display purposes.
	Name string `json:"name,omitempty" jsonschema:"description=Human-readable name for the provider,example=OpenAI"`
	// The provider's API endpoint.
	BaseURL string `json:"base_url,omitempty" jsonschema:"description=Base URL for the provider's API,format=uri,example=https://api.openai.com/v1"`
	// The provider's proxy URL (http/https/socks5). The value runs
	// through shell expansion at config-load time, the same as api_key
	// and extra_headers, so $VAR and $(cmd) work. Empty means no
	// per-provider proxy override — requests fall back to the standard
	// HTTP_PROXY/HTTPS_PROXY/NO_PROXY environment variables via net/http's
	// default proxy resolution.
	//
	// This is the EFFECTIVE proxy: everywhere in Sennit that sends a
	// request reads this field and this field alone, so a switch to an
	// account with its own proxy is published here (see
	// ConfigStore.UpdateProviderAccount), not through a second field
	// every call site would have had to learn to also check. See
	// ConfiguredProxyURL for the value this is computed from.
	ProxyURL string `json:"proxy_url,omitempty" jsonschema:"description=Proxy URL for requests to this provider (http/https/socks5); set to \"none\" to force a direct connection even if HTTP_PROXY/HTTPS_PROXY are set in the environment,example=http://localhost:8080"`
	// ConfiguredProxyURL is the provider-level proxy exactly as
	// configured (i.e. what ProxyURL held before any account ever
	// overrode it) — the base that UpdateProviderAccount resolves the
	// effective ProxyURL from on every account switch. Without it, a
	// switch away from an account with its own proxy to one with none
	// would have nothing to fall back to except whatever ProxyURL
	// happened to hold at that moment — the PREVIOUS account's proxy —
	// instead of the provider's own. Set once, at load time, alongside
	// ProxyURL (see providerload); never serialized, since it is derived
	// from the same source ProxyURL already reads from disk.
	ConfiguredProxyURL string `json:"-"`
	// The provider type, e.g. "openai", "anthropic", etc. if empty it defaults to openai.
	Type catwalk.Type `json:"type,omitempty" jsonschema:"description=Provider type that determines the API format,default=openai"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
	// The original API key template before resolution (for re-resolution on auth errors).
	APIKeyTemplate string `json:"-"`
	// OAuthToken for providers that use OAuth2 authentication.
	OAuthToken *oauth.Token `json:"oauth,omitempty" jsonschema:"description=OAuth2 token for authentication with the provider"`
	// Marks the provider as disabled.
	Disable bool `json:"disable,omitempty" jsonschema:"description=Whether this provider is disabled,default=false"`

	// Account is the ID of the account (see internal/providers/accounts)
	// whose credentials this provider entry currently carries. The
	// APIKey/OAuthToken/ProxyURL fields above are a projection of that
	// account onto this config entry, not independent state; a provider
	// with only one account may leave this empty. Written by
	// ConfigStore.ActivateAccount and read wherever the active account
	// has to be identified (see internal/config/provider_accounts.go).
	Account string `json:"account,omitempty" jsonschema:"description=ID of the active account for this provider, if it has more than one"`

	// Rotation configures automatic switching between this provider's
	// stored accounts (see internal/providers/accounts). It is a pointer
	// so "not configured" (nil, rotation off, no fields to validate) and
	// "configured with every field at its zero value" stay
	// distinguishable — the same reasoning as OAuthToken above.
	Rotation *RotationConfig `json:"rotation,omitempty" jsonschema:"description=Automatic account rotation settings for this provider"`

	// Custom system prompt prefix.
	SystemPromptPrefix string `json:"system_prompt_prefix,omitempty" jsonschema:"description=Custom prefix to add to system prompts for this provider"`

	// Extra headers to send with each request to the provider. Values
	// run through shell expansion at config-load time, so $VAR and
	// $(cmd) work the same way they do in MCP headers. A header whose
	// value resolves to the empty string (unset bare $VAR under
	// lenient nounset, $(echo), or literal "") is omitted from the
	// outgoing request rather than sent as "Header:".
	ExtraHeaders map[string]string `json:"extra_headers,omitempty" jsonschema:"description=Additional HTTP headers to send with requests"`
	// ExtraBody is merged verbatim into OpenAI-compatible request
	// bodies. String values are NOT shell-expanded: this is a plain
	// JSON passthrough so that arbitrary provider-extension fields
	// (numbers, nested objects, booleans) round-trip without a
	// recursive walker guessing at intent. If you need an env-var-
	// driven value at request time, put it in extra_headers, or in
	// the provider's top-level api_key / base_url, all of which do
	// expand.
	ExtraBody map[string]any `json:"extra_body,omitempty" jsonschema:"description=Additional fields to include in request bodies\\, only works with openai-compatible providers"`

	ProviderOptions map[string]any `json:"provider_options,omitempty" jsonschema:"description=Additional provider-specific options for this provider"`

	// Used to pass extra parameters to the provider.
	ExtraParams map[string]string `json:"-"`

	// AWSAuthRefresh is a shell command run when Bedrock returns a
	// credential error. Output is discarded to avoid corrupting the TUI.
	AWSAuthRefresh string `json:"aws_auth_refresh,omitempty" jsonschema:"description=Shell command to run when AWS credentials expire (Bedrock only)."`

	// Skip cost accumulation for this provider when using subscription or flat rate billing.
	FlatRate bool `json:"flat_rate,omitempty" jsonschema:"description=Flat-rate mode for this provider"`

	// AutoDiscoverModels controls model discovery via /v1/models endpoint.
	// When Models is empty and this is nil or true, Sennit auto-discovers
	// models. When true and Models is non-empty, discovered models are
	// merged in (user-specified models take precedence). When false,
	// only explicitly listed models are used.
	AutoDiscoverModels *bool `json:"discover_models,omitempty" jsonschema:"description=Auto-discover models from /v1/models endpoint. When true with existing models they are merged (yours win),default=true"`

	// The provider models
	Models []catwalk.Model `json:"models,omitempty" jsonschema:"description=List of models available from this provider"`

	// ModelsSource records where Models came from for this load: the
	// user's own config, or the global model-discovery cache (see
	// internal/config/modelcache.go). It is in-memory bookkeeping only,
	// never serialized — set by resolveCustomProviderModels/
	// validateCustomProviders in load.go, and read by `sennit models
	// refresh` (internal/cmd/models.go) to refuse silently overwriting a
	// manually curated list with (possibly junk) discovery output.
	ModelsSource ModelsSource `json:"-"`
}

// ModelsSource identifies where a custom provider's Models list came from.
type ModelsSource string

const (
	// ModelsSourceConfig means Models was written by hand in sennitrc/
	// sennit.json — refresh must never overwrite it silently.
	ModelsSourceConfig ModelsSource = "config"
	// ModelsSourceCache means Models came from discovery, either just now
	// or from a previous load via the global model-discovery cache.
	ModelsSourceCache ModelsSource = "cache"
)

// RotationConfig configures automatic switching between a provider's
// stored accounts (see internal/providers/accounts) once the active one
// runs low or gets rate-limited. This step only stores and validates the
// setting — the rotator that actually acts on it is a later change.
//
// Which fields apply depends on accounts.CapabilitiesOf(providerID).RotateOn:
// a threshold provider (Codex: it reports remaining allowance) rotates
// when the active account's remaining allowance drops below
// MinRemainingPercent; a rate-limit provider (everyone else) has no
// number to compare against and instead rotates reactively on HTTP 429,
// waiting Cooldown before trying an account again if the response carried
// no Retry-After header. Setting the field that doesn't apply to a given
// provider is a config error — see providerload's rotation validation.
type RotationConfig struct {
	// Enabled turns automatic rotation on for this provider. Meaningful
	// for both RotateOn kinds.
	Enabled bool `json:"enabled,omitempty" jsonschema:"description=Automatically rotate to another account for this provider,default=false"`
	// MinRemainingPercent is the remaining-allowance threshold that
	// triggers rotation, as a percentage (1-99). Valid only for
	// providers whose RotateOn is accounts.RotateThreshold; zero means
	// "use accounts.DefaultMinRemainingPercent" (see
	// EffectiveMinRemainingPercent).
	MinRemainingPercent int `json:"min_remaining_percent,omitempty" jsonschema:"description=Rotate once remaining allowance drops below this percentage (1-99); only valid for providers that report remaining allowance,minimum=1,maximum=99"`
	// Cooldown is how long an account is treated as exhausted after a
	// 429 with no Retry-After header, as a Go duration string (e.g.
	// "10m"). Valid only for providers whose RotateOn is
	// accounts.RotateRateLimit; empty means "use accounts.DefaultCooldown"
	// (see EffectiveCooldown).
	Cooldown string `json:"cooldown,omitempty" jsonschema:"description=How long to treat a rate-limited account as exhausted before retrying it\\, as a Go duration (e.g. 10m); only valid for providers with no remaining-allowance reporting,example=10m"`
	// Order lists account IDs in the order rotation should try them.
	// Empty means "the order accounts were added in."
	Order []string `json:"order,omitempty" jsonschema:"description=Account IDs in the order rotation should try them; empty means the order they were added in"`
}

// EffectiveMinRemainingPercent returns the threshold rotation should use:
// r.MinRemainingPercent when set, otherwise
// accounts.DefaultMinRemainingPercent. Safe to call on a nil r.
func (r *RotationConfig) EffectiveMinRemainingPercent() int {
	if r == nil || r.MinRemainingPercent == 0 {
		return accounts.DefaultMinRemainingPercent
	}
	return r.MinRemainingPercent
}

// EffectiveCooldown returns the cooldown rotation should use: r.Cooldown
// parsed as a duration when set, otherwise accounts.DefaultCooldown. Safe
// to call on a nil r. An unparseable Cooldown also falls back to the
// default rather than erroring — providerload's validation is what keeps
// a bad value from ever reaching here in practice.
func (r *RotationConfig) EffectiveCooldown() time.Duration {
	if r == nil || r.Cooldown == "" {
		return accounts.DefaultCooldown
	}
	d, err := time.ParseDuration(r.Cooldown)
	if err != nil || d <= 0 {
		return accounts.DefaultCooldown
	}
	return d
}

// ToProvider converts the [ProviderConfig] to a [catwalk.Provider].
func (c *ProviderConfig) ToProvider() catwalk.Provider {
	// Convert config provider to provider.Provider format
	provider := catwalk.Provider{
		Name:   c.Name,
		ID:     catwalk.InferenceProvider(c.ID),
		Models: make([]catwalk.Model, len(c.Models)),
	}

	// Convert models
	for i, model := range c.Models {
		provider.Models[i] = catwalk.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        model.ReasoningLevels,
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
		}
	}

	return provider
}

// SetupGitHubCopilot adds the headers Copilot requires to the provider.
//
// The map is created when absent: a provider declared without extra_headers
// decodes with a nil map, and copying into a nil map panics. That is reachable
// from the OAuth refresh path, where a Copilot entry holding only a token would
// take down the process.
func (c *ProviderConfig) SetupGitHubCopilot() {
	if c.ExtraHeaders == nil {
		c.ExtraHeaders = make(map[string]string)
	}
	maps.Copy(c.ExtraHeaders, copilot.Headers())
}

// SetupCodex adds the headers the Codex backend requires to the provider.
//
// The account header is derived from the access token rather than stored:
// the token is a JWT that names the account it was issued for, so a token
// refresh or an account switch carries the right value automatically, and
// nothing extra has to be kept in sync on disk.
func (c *ProviderConfig) SetupCodex() {
	if c.ExtraHeaders == nil {
		c.ExtraHeaders = make(map[string]string)
	}
	accountID := codex.AccountID(c.APIKey)
	if accountID == "" && c.OAuthToken != nil {
		accountID = codex.AccountID(c.OAuthToken.AccessToken)
	}
	// codex.Headers omits the account header entirely when accountID is
	// "" (personal-plan tokens carry no chatgpt_account_id claim), and
	// maps.Copy only ever adds or overwrites — it never deletes. Left
	// alone, a switch from an account whose token names one to an
	// account whose token doesn't would leave the PREVIOUS account's
	// header sitting in ExtraHeaders, so the backend would keep acting
	// on its behalf instead of falling back to the token's own account
	// as it should. Delete it explicitly so an unclaimed accountID
	// really does mean "let the backend decide," not "whoever asked
	// last."
	if accountID == "" {
		delete(c.ExtraHeaders, codex.AccountIDHeader)
	}
	maps.Copy(c.ExtraHeaders, codex.Headers(accountID))
}

// Providers returns the provider catalog for cfg.
//
// It used to memoize the result process-globally via sync.Once, but that
// cached the first cfg it ever saw and kept returning that answer to every
// other *Config passed in afterwards — fatal once a single process can hold
// several ConfigStores (e.g. one per workspace) with different
// DisableDefaultProviders settings. embedded.GetAll() is just an in-memory
// slice, so recomputing per call is cheap; callers that already hold a
// ConfigStore should prefer its cached ConfigStore.KnownProviders() instead
// of calling this repeatedly.
func Providers(cfg *Config) []catwalk.Provider {
	// The provider catalog ships with the binary. Upstream refreshed it
	// from Charm's catwalk service at startup; that call is removed, so
	// Sennit never reaches the network to decide what models exist.
	// Anything the embedded list does not know about is declared in the
	// user's own config.
	if cfg.Options.DisableDefaultProviders {
		return nil
	}
	return append(embedded.GetAll(), CodexProvider())
}

// CodexProvider is the catalog entry for OpenAI Codex.
//
// Codex is not in the embedded catalog: it is not an API-key provider, it is
// what a ChatGPT subscription unlocks, so it only exists once the user signs
// in. Declaring it here rather than writing an endpoint and a type into the
// user's config at sign-in time means a later Sennit can move the endpoint
// or change the headers without every existing install being stuck on the
// values it was set up with.
//
// The model list stays empty on purpose: which models an account may use is
// per-plan and changes over time, so it is fetched from OpenAI's Codex API at
// sign-in (see oauth/codex.FetchModels) and merged in from the user's config
// (see mergeCatalogProviders).
func CodexProvider() catwalk.Provider {
	return catwalk.Provider{
		ID:          catwalk.InferenceProvider(codex.ProviderID),
		Name:        codex.ProviderName,
		APIEndpoint: codex.APIBaseURL,
		// The Codex endpoint speaks the Responses API, which is exactly
		// what the OpenAI provider type sends.
		Type: catwalk.TypeOpenAI,
	}
}

func (c *ProviderConfig) TestConnection(resolver VariableResolver) error {
	var (
		providerID = catwalk.InferenceProvider(c.ID)
		testURL    = ""
		headers    = make(map[string]string)
		apiKey, _  = resolver.ResolveValue(c.APIKey)
	)

	switch providerID {
	case catwalk.InferenceProviderMiniMax, catwalk.InferenceProviderMiniMaxChina:
		// NOTE: MiniMax has no good endpoint we can use to validate the API key.
		return nil
	case catwalk.InferenceProviderAlibabaSingapore:
		// NOTE: Alibaba has no good endpoint we can use to validate the API key.
		// Let's at least check the pattern.
		if !strings.HasPrefix(apiKey, "sk-") {
			return fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return nil
	}

	switch c.Type {
	case catwalk.TypeOpenAI, catwalk.TypeOpenAICompat, catwalk.TypeOpenRouter:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.openai.com/v1")

		switch providerID {
		case catwalk.InferenceProviderOpenRouter:
			testURL = baseURL + "/credits"
		case catwalk.InferenceProviderOpenCodeGo:
			testURL = strings.Replace(baseURL, "/go", "", 1) + "/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["Authorization"] = "Bearer " + apiKey
	case catwalk.TypeAnthropic:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://api.anthropic.com/v1")

		switch providerID {
		case catwalk.InferenceKimiCoding:
			testURL = baseURL + "/v1/models"
		default:
			testURL = baseURL + "/models"
		}

		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = "2023-06-01"
	case catwalk.TypeGoogle:
		baseURL, _ := resolver.ResolveValue(c.BaseURL)
		baseURL = cmp.Or(baseURL, "https://generativelanguage.googleapis.com")
		// The key goes in a header, never the URL: a failed request wraps
		// the full URL into the returned error (*url.Error), which ends up
		// in the UI and logs.
		testURL = baseURL + "/v1beta/models"
		headers["x-goog-api-key"] = apiKey
	case catwalk.TypeBedrock:
		// NOTE: Bedrock has a `/foundation-models` endpoint that we could in
		// theory use, but apparently the authorization is region-specific,
		// so it's not so trivial.
		if strings.HasPrefix(apiKey, "ABSK") { // Bedrock API keys
			return nil
		}
		return errors.New("not a valid bedrock api key")
	case catwalk.TypeVercel:
		// NOTE: Vercel does not validate API keys on the `/models` endpoint.
		if strings.HasPrefix(apiKey, "vck_") { // Vercel API keys
			return nil
		}
		return errors.New("not a valid vercel api key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := &http.Client{}
	req, err := http.NewRequestWithContext(ctx, "GET", testURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for k, v := range c.ExtraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()

	switch providerID {
	case catwalk.InferenceProviderZAI:
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	default:
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
	}
	return nil
}
