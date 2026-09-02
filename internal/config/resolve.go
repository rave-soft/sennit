package config

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/rave-soft/sennit/internal/env"
	providerconfig "github.com/rave-soft/sennit/internal/providers/config"
	"github.com/rave-soft/sennit/internal/shell"
)

// resolveTimeout bounds how long a single ResolveValue call may spend
// inside shell expansion (including any command substitution).
const resolveTimeout = 5 * time.Minute

// VariableResolver and IdentityResolver live in internal/providers/config
// now, for the same import-cycle reason as ProviderConfig (see
// internal/config/provider.go). shellVariableResolver below satisfies
// VariableResolver structurally and needs no change to do so.
type VariableResolver = providerconfig.VariableResolver

var IdentityResolver = providerconfig.IdentityResolver

// Expander is the single-value shell expansion seam used by
// shellVariableResolver. Production wires it to shell.ExpandValue; tests
// can inject a fake via WithExpander.
type Expander func(ctx context.Context, value string, env []string) (string, error)

// ShellResolverOption customizes shell variable resolver construction.
type ShellResolverOption func(*shellVariableResolver)

// WithExpander overrides the expansion function used by the resolver.
// Primarily intended for tests; production callers should not need this.
func WithExpander(e Expander) ShellResolverOption {
	return func(r *shellVariableResolver) {
		if e != nil {
			r.expand = e
		}
	}
}

type shellVariableResolver struct {
	env    env.Env
	expand Expander
}

// NewShellVariableResolver returns a VariableResolver that delegates to
// the embedded shell (the same interpreter used by the bash tool and
// hooks). Supported constructs match shell.ExpandValue: $VAR, ${VAR},
// ${VAR:-default}, $(command), quoting, and escapes. Unset variables
// expand to the empty string by default, matching bash; use
// ${VAR:?message} to require a value and fail loudly when it is missing.
// The stricter "unset is always an error" mode is gated globally by
// shell.NoUnset.
func NewShellVariableResolver(e env.Env, opts ...ShellResolverOption) VariableResolver {
	r := &shellVariableResolver{
		env:    e,
		expand: shell.ExpandValue,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// ResolveValue resolves shell-style substitution anywhere in the string:
//
//   - $(command) for command substitution, with full quoting and nesting.
//   - $VAR and ${VAR} for environment variables.
//   - ${VAR:-default} / ${VAR:+alt} / ${VAR:?msg} for defaulting.
//
// Unset variables expand to the empty string by default, matching bash.
// Command-substitution failures are always a hard error. Required
// credentials should use ${VAR:?message} so a missing variable fails
// loudly at load time instead of quietly resolving to empty. Global
// strict mode is available via shell.NoUnset for callers that want the
// old nounset-on behaviour back.
func (r *shellVariableResolver) ResolveValue(value string) (string, error) {
	// Preserve the historical backward-compat contract: a lone "$" is a
	// malformed config value, not a legal literal. The underlying shell
	// parser would accept it as a literal; we reject it here so existing
	// configs that relied on this validation still fail early.
	if value == "$" {
		return "", fmt.Errorf("invalid value format: %s", value)
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()

	out, err := r.expand(ctx, value, r.env.Env())
	if err != nil {
		return "", sanitizeResolveError(value, err)
	}
	return out, nil
}

// maxResolveErrBytes bounds the size of the inner error message surfaced
// from a resolution failure. Defense-in-depth on top of shell.ExpandValue's
// own stderr budget: a custom Expander injected via WithExpander, or any
// future non-shell error path, must still produce a user-safe message.
const maxResolveErrBytes = 512

// sanitizeResolveError wraps an expansion error with the user-written
// template (the pre-expansion string — it is what they typed, safe to
// surface) and a bounded, scrubbed rendering of the inner error message.
// Contract:
//
//   - Never includes the resolved (post-expansion) value. This helper
//     only receives the template and err, so a successful expansion
//     result cannot reach it.
//   - May include the template verbatim.
//   - Truncates the inner error's message to maxResolveErrBytes and
//     replaces embedded NULs and other non-printables (except tab and
//     newline) with '?'.
//
// The returned error still unwraps to the original for errors.Is/As so
// callers can inspect typed sentinels; only the rendered message is
// scrubbed.
func sanitizeResolveError(template string, err error) error {
	if err == nil {
		return nil
	}
	return &resolveError{
		template: template,
		msg:      scrubErrorMessage(err.Error()),
		inner:    err,
	}
}

// resolveError is the concrete type returned by sanitizeResolveError.
// Its Error() method returns the template + scrubbed inner message;
// Unwrap exposes the original error so errors.Is/As continue to work.
type resolveError struct {
	template string
	msg      string
	inner    error
}

func (e *resolveError) Error() string {
	return fmt.Sprintf("resolving %q: %s", e.template, e.msg)
}

func (e *resolveError) Unwrap() error { return e.inner }

// scrubErrorMessage bounds the message to maxResolveErrBytes bytes and
// replaces non-printable bytes (anything outside ASCII printable, tab, or
// newline) with '?'. Mirrors shell.sanitizeStderr but operates on a
// string rather than raw command stderr and runs at the config layer,
// so arbitrary Expander error text is also sanitized.
func scrubErrorMessage(s string) string {
	if len(s) > maxResolveErrBytes {
		s = s[:maxResolveErrBytes]
	}
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\t' || c == '\n' || (c >= 0x20 && c < 0x7f) {
			out[i] = c
			continue
		}
		out[i] = '?'
	}
	return string(out)
}

// ResolvedEnv returns m.Env with every value expanded through the
// given resolver. The returned slice is of the form "KEY=value" sorted
// by key so callers get deterministic output; the receiver's Env map is
// not mutated. On the first resolution failure it returns nil and an
// error that identifies the offending key; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Callers are expected to surface it
// (for MCP, via StateError on the status card) rather than silently
// spawn the server with an empty credential.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim and expansion happens on the server.
func (m MCPConfig) ResolvedEnv(r VariableResolver) ([]string, error) {
	return resolveEnvs(m.Env, r)
}

// ResolvedArgs returns m.Args with every element expanded through the
// given resolver. A fresh slice is allocated; m.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	return resolveSlice(m.Args, r)
}

// ResolvedURL returns m.URL expanded through the given resolver. The
// receiver is not mutated. Errors from the resolver are already
// sanitized by ResolveValue and are wrapped with %w for errors.Is/As.
//
// URLs run through the same shell-expansion pipeline as the other
// fields, so a literal '$' (e.g. OData query strings containing
// $filter/$select) must be escaped as '\$' or '${DOLLAR:-$}' to avoid
// being interpreted as a variable reference. Same constraint already
// applies to command, args, env, and headers.
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedURL(r VariableResolver) (string, error) {
	if m.URL == "" {
		return "", nil
	}
	v, err := r.ResolveValue(m.URL)
	if err != nil {
		return "", fmt.Errorf("url: %w", err)
	}
	return v, nil
}

// ResolvedHeaders returns m.Headers with every value expanded through
// the given resolver. A fresh map is allocated; m.Headers is never
// mutated. On the first resolution failure it returns nil and an error
// identifying the offending header name; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// A header whose value resolves to the empty string (unset bare $VAR
// under lenient nounset, $(echo), or literal "") is omitted from the
// returned map — sending "X-Auth:" with an empty value is rejected by
// some providers and the user's intent in "optional, env-gated
// header" is clearly "absent when the var isn't set."
//
// See ResolvedEnv for guidance on picking a resolver.
func (m MCPConfig) ResolvedHeaders(r VariableResolver) (map[string]string, error) {
	return resolveMap(m.Headers, r, func(k string) string { return "header " + k }, true)
}

// ResolvedArgs returns l.Args with every element expanded through the
// given resolver. A fresh slice is allocated; l.Args is never mutated.
// On the first resolution failure it returns nil and an error
// identifying the offending positional index; the inner resolver error
// is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work.
//
// Empty resolved values are kept (a deliberate "empty positional arg"
// like --flag "" is sometimes valid), matching MCPConfig.ResolvedArgs.
//
// The resolver choice matters: in server mode pass the shell resolver
// so $VAR / $(cmd) expand; in client mode pass IdentityResolver so the
// template is forwarded verbatim.
func (l LSPConfig) ResolvedArgs(r VariableResolver) ([]string, error) {
	return resolveSlice(l.Args, r)
}

// ResolvedEnv returns l.Env with every value expanded through the
// given resolver. A fresh map is allocated; l.Env is never mutated.
// On the first resolution failure it returns nil and an error that
// identifies the offending key; the inner resolver error is already
// sanitized by ResolveValue and is wrapped with %w so errors.Is/As
// continues to work.
//
// Empty resolved values are kept ("FOO=" is a legitimate request;
// opt out via ${VAR:+...}), matching MCPConfig.ResolvedEnv.
//
// Shape note: this returns map[string]string rather than the []string
// shape MCPConfig.ResolvedEnv uses because the consumer
// (powernap.ClientConfig.Environment in internal/lsp/client.go) takes
// a map directly — returning a []string here would only force a
// round-trip back to a map at the call site.
//
// See ResolvedArgs for guidance on picking a resolver.
func (l LSPConfig) ResolvedEnv(r VariableResolver) (map[string]string, error) {
	return resolveMap(l.Env, r, func(k string) string { return fmt.Sprintf("env %q", k) }, false)
}

// resolveEnvs expands every value in envs through the given resolver
// and returns a fresh "KEY=value" slice sorted by key. The input map is
// not mutated. On the first resolution failure it returns nil and an
// error identifying the offending variable; the inner resolver error is
// already sanitized by ResolveValue and is wrapped with %w.
func resolveEnvs(envs map[string]string, r VariableResolver) ([]string, error) {
	if len(envs) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(envs))
	for k := range envs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res := make([]string, 0, len(envs))
	for _, k := range keys {
		v, err := r.ResolveValue(envs[k])
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res, nil
}

// resolveSlice resolves every element of items through r, in order,
// returning a fresh slice (items is never mutated); a nil/empty input
// returns nil, nil. On the first resolution failure it returns nil and an
// error identifying the offending positional index; the inner resolver
// error is already sanitized by ResolveValue and is wrapped with %w so
// errors.Is/As continues to work. Shared by MCPConfig.ResolvedArgs and
// LSPConfig.ResolvedArgs, which agree on every aspect of this contract
// (including keeping empty resolved values, unlike resolveMap's
// dropEmpty option).
func resolveSlice(items []string, r VariableResolver) ([]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]string, len(items))
	for i, a := range items {
		v, err := r.ResolveValue(a)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// resolveMap resolves every value of m through r, visiting keys in sorted
// order so a failure is reported deterministically when more than one
// value would fail; a nil/empty input returns an empty, non-nil map. The
// input map is never mutated. errKey builds the error-message prefix for
// the offending key on failure (callers disagree on quoting, e.g. "header
// X" vs. `env "X"`, so this is left to them rather than baked in here).
// dropEmpty omits a key whose resolved value is "" — MCPConfig.ResolvedHeaders
// wants that (some providers reject an empty header value); LSPConfig.ResolvedEnv
// does not (an explicit "FOO=" is a legitimate request).
func resolveMap(m map[string]string, r VariableResolver, errKey func(key string) string, dropEmpty bool) (map[string]string, error) {
	return providerconfig.ResolveMap(m, r, errKey, dropEmpty)
}
