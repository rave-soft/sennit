package accounts

// RotateOn says what conditions, if any, make Sennit offer to rotate to a
// different account for a provider. It exists to make "the minimum-limit
// threshold is only meaningful where a limit is actually reported" a
// structural property rather than a runtime check scattered across
// callers: a threshold setting is read and validated only when
// RotatesOnThreshold, a cooldown setting only when RotatesOnRateLimit. A
// provider that reports no usage has no threshold to configure, because
// there is nothing to measure it against until a 429 actually arrives.
type RotateOn int

const (
	// RotateNever means rotation is never offered for this provider.
	RotateNever RotateOn = iota
	// RotateThreshold means rotation is offered proactively, once the
	// remaining allowance drops below a configurable threshold.
	RotateThreshold
	// RotateRateLimit means rotation is offered reactively, on a 429
	// response.
	RotateRateLimit
	// RotateBoth means rotation is offered on both conditions:
	// proactively once remaining allowance drops below the threshold,
	// and reactively on a 429.
	RotateBoth
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
	case RotateBoth:
		return "both"
	default:
		return "unknown"
	}
}

// RotatesOnThreshold reports whether a usage threshold should trigger a
// rotation offer for this provider.
func (r RotateOn) RotatesOnThreshold() bool {
	return r == RotateThreshold || r == RotateBoth
}

// RotatesOnRateLimit reports whether a 429 response should trigger a
// rotation offer for this provider.
func (r RotateOn) RotatesOnRateLimit() bool {
	return r == RotateRateLimit || r == RotateBoth
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
	// provider-specific package. RotateBoth: the proactive threshold
	// path only works when a fresh usage snapshot is available in-process
	// (the usage transport's in-memory store), which is not guaranteed at
	// the moment a limit is hit, so the reactive 429 path is kept
	// alongside it as a safety net.
	"codex": {Usage: true, RotateOn: RotateBoth, AuthKind: AuthOAuth},
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
