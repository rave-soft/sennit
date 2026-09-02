package providerconfig

import (
	"fmt"
	"slices"
)

// VariableResolver resolves a single config-string value (e.g. "$VAR" or
// "$(cmd)") to its final form. internal/config's shellVariableResolver is
// the production implementation; it satisfies this interface structurally
// and does not need to import this package to do so.
type VariableResolver interface {
	ResolveValue(value string) (string, error)
}

// identityResolver is a no-op resolver that returns values unchanged.
// Used in client mode where variable resolution is handled server-side.
type identityResolver struct{}

func (identityResolver) ResolveValue(value string) (string, error) {
	return value, nil
}

// IdentityResolver returns a VariableResolver that passes values through
// unchanged.
func IdentityResolver() VariableResolver {
	return identityResolver{}
}

// ResolveMap resolves every value of m through r, visiting keys in sorted
// order so a failure is reported deterministically when more than one
// value would fail; a nil/empty input returns an empty, non-nil map. The
// input map is never mutated. errKey builds the error-message prefix for
// the offending key on failure. dropEmpty omits a key whose resolved
// value is "".
//
// This is internal/config's resolveMap, exported here so
// ResolveProviderHeaders can use it without internal/config needing to
// import this package back (which would recreate the cycle this package
// exists to avoid). internal/config's own resolveMap forwards to this one.
func ResolveMap(m map[string]string, r VariableResolver, errKey func(key string) string, dropEmpty bool) (map[string]string, error) {
	if len(m) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, err := r.ResolveValue(m[k])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", errKey(k), err)
		}
		if dropEmpty && v == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}
