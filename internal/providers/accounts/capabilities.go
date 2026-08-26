package accounts

// RotateOn says what condition, if any, makes Sennit offer to rotate to a
// different account for a provider. It exists to make "the minimum-limit
// threshold is only meaningful where a limit is actually reported" a
// structural property rather than a runtime check scattered across
// callers: a threshold setting is read and validated only when RotateOn
// is RotateThreshold. A provider stuck at RotateRateLimit has no
// threshold to configure, because there is nothing to measure it against
// until a 429 actually arrives.
type RotateOn int

const (
	// RotateNever means rotation is never offered for this provider.
	RotateNever RotateOn = iota
	// RotateThreshold offers rotation once remaining allowance drops
	// below a configurable threshold. Only valid where Capabilities.Usage
	// is true — there has to be a number to compare against.
	RotateThreshold
	// RotateRateLimit offers rotation reactively, on a 429 response.
	// There is no threshold to configure: the signal is binary.
	RotateRateLimit
)

// String implements fmt.Stringer for log messages and error text.
func (r RotateOn) String() string {
	switch r {
	case RotateNever:
		return "never"
	case RotateThreshold:
		return "threshold"
	case RotateRateLimit:
		return "rate-limit"
	default:
		return "unknown"
	}
}

// AuthKind is how a provider's accounts authenticate.
type AuthKind int

const (
	// AuthAPIKey accounts carry a resolved-at-use-time API key template.
	AuthAPIKey AuthKind = iota
	// AuthOAuth accounts carry an oauth.Token.
	AuthOAuth
)

// String implements fmt.Stringer for log messages and error text.
func (k AuthKind) String() string {
	switch k {
	case AuthAPIKey:
		return "api-key"
	case AuthOAuth:
		return "oauth"
	default:
		return "unknown"
	}
}

// Capabilities describes what multi-account support looks like for a
// given provider: whether it reports remaining allowance, what should
// trigger an account rotation, and how its accounts authenticate.
type Capabilities struct {
	// Usage reports whether the provider tells Sennit how much of the
	// account's allowance remains.
	Usage bool
	// RotateOn is the condition that makes rotation worth offering.
	RotateOn RotateOn
	// AuthKind is how the provider's accounts authenticate.
	AuthKind AuthKind
}

// capabilities is the registry of known providers. Anything not listed
// here falls back to the zero-configuration case: an API-key provider
// with no usage reporting, rotated reactively on rate-limit errors.
var capabilities = map[string]Capabilities{
	// "codex" is codex.ProviderID (internal/oauth/codex), spelled as a
	// literal so this leaf package does not have to import that
	// provider-specific package.
	"codex": {Usage: true, RotateOn: RotateThreshold, AuthKind: AuthOAuth},
	// "copilot" is catwalk.InferenceProviderCopilot, spelled as a
	// literal for the same reason.
	"copilot": {Usage: false, RotateOn: RotateRateLimit, AuthKind: AuthOAuth},
}

// defaultCapabilities is what CapabilitiesOf returns for any provider not
// in the registry above.
var defaultCapabilities = Capabilities{
	Usage:    false,
	RotateOn: RotateRateLimit,
	AuthKind: AuthAPIKey,
}

// CapabilitiesOf returns the multi-account capabilities for providerID.
// Unknown providers get defaultCapabilities rather than an error, so
// callers can call this unconditionally without a provider allowlist.
func CapabilitiesOf(providerID string) Capabilities {
	if c, ok := capabilities[providerID]; ok {
		return c
	}
	return defaultCapabilities
}
