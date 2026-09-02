// Package providerconfig holds the provider-configuration types and
// variable-resolution machinery shared between internal/config and
// internal/providers/runtime.
//
// It exists to break an import cycle: internal/providers/runtime needs
// ProviderConfig, VariableResolver, and the header/proxy resolution
// helpers below, but internal/config needs to call into
// internal/providers/runtime directly (see internal/config/store_credentials.go
// and internal/config/runtime.go). Two packages cannot import each other,
// so the types both sides need live here instead, in a leaf package
// neither internal/config nor internal/providers/runtime need fear
// depending on.
//
// internal/config re-exports every type here under its historical name
// (ProviderConfig = providerconfig.ProviderConfig, and so on) via type
// aliases, so the ~50 packages across the tree that already say
// config.ProviderConfig do not need to change.
package providerconfig

import (
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/oauth"
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
	ProxyURL string `json:"proxy_url,omitempty" jsonschema:"description=Proxy URL for requests to this provider (http/https/socks5); set to \"none\" to force a direct connection even if HTTP_PROXY/HTTPS_PROXY are set in the environment,example=http://localhost:8080"`
	// The provider type, e.g. "openai", "anthropic", etc. if empty it defaults to openai.
	Type catwalk.Type `json:"type,omitempty" jsonschema:"description=Provider type that determines the API format,default=openai"`
	// The provider's API key.
	APIKey string `json:"api_key,omitempty" jsonschema:"description=API key for authentication with the provider,example=$OPENAI_API_KEY"`
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
	// internal/modelcache). It is in-memory bookkeeping only,
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
