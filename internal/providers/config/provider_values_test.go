package providerconfig

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type mapResolver map[string]string

func (r mapResolver) ResolveValue(value string) (string, error) {
	if resolved, ok := r[value]; ok {
		return resolved, nil
	}
	return value, nil
}

type failingResolver struct{ err error }

func (r failingResolver) ResolveValue(string) (string, error) { return "", r.err }

func TestResolveProviderHeadersResolvesAndDropsEmpty(t *testing.T) {
	headers := map[string]string{"X-Static": "value", "X-Var": "$VAR", "X-Empty": "$EMPTY"}
	resolver := mapResolver{"$VAR": "resolved", "$EMPTY": ""}

	require.NoError(t, ResolveProviderHeaders(headers, resolver, "test"))
	require.Equal(t, map[string]string{"X-Static": "value", "X-Var": "resolved"}, headers)
}

func TestResolveProviderHeadersPropagatesResolverError(t *testing.T) {
	want := errors.New("boom")
	headers := map[string]string{"X-Var": "$VAR"}
	err := ResolveProviderHeaders(headers, failingResolver{err: want}, "test")
	require.ErrorIs(t, err, want)
}

func TestResolveOptionalProviderProxyEmptyInputStaysEmpty(t *testing.T) {
	require.Equal(t, "", ResolveOptionalProviderProxy("", mapResolver{}, "test"))
}

func TestResolveOptionalProviderProxyResolvesValue(t *testing.T) {
	resolver := mapResolver{"$PROXY": "http://proxy.example:8080"}
	require.Equal(t, "http://proxy.example:8080", ResolveOptionalProviderProxy("$PROXY", resolver, "test"))
}

func TestResolveOptionalProviderProxyFailureFallsBackToEmpty(t *testing.T) {
	got := ResolveOptionalProviderProxy("$PROXY", failingResolver{err: errors.New("boom")}, "test")
	require.Equal(t, "", got, "a resolution failure must not propagate as an error; it disables the override instead")
}

func TestIdentityResolverPassesThroughLiterals(t *testing.T) {
	got, err := IdentityResolver().ResolveValue("$VAR")
	require.NoError(t, err)
	require.Equal(t, "$VAR", got)
}
