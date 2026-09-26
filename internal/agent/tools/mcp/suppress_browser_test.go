package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShouldSuppressBrowser covers the two independent ways a connect can
// be told not to open a local browser (see shouldSuppressBrowser's doc
// comment): a per-call ctx marker (BeginAuth's server-driven remote auth),
// and Registry.SetSuppressBrowser applying to every connect the Registry
// makes (a daemon, CLIENT-SERVER.md PR 2.1). Neither is required for the
// other; either alone is enough.
func TestShouldSuppressBrowser(t *testing.T) {
	t.Parallel()

	suppressedCtx := context.WithValue(t.Context(), suppressBrowserKey{}, true)

	for _, tc := range []struct {
		name           string
		ctx            context.Context
		registrySet    bool
		wantSuppressed bool
	}{
		{name: "neither set", ctx: t.Context(), registrySet: false, wantSuppressed: false},
		{name: "ctx marker only", ctx: suppressedCtx, registrySet: false, wantSuppressed: true},
		{name: "registry flag only", ctx: t.Context(), registrySet: true, wantSuppressed: true},
		{name: "both set", ctx: suppressedCtx, registrySet: true, wantSuppressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRegistry()
			if tc.registrySet {
				r.SetSuppressBrowser(true)
			}
			require.Equal(t, tc.wantSuppressed, shouldSuppressBrowser(tc.ctx, r))
		})
	}
}

// TestRegistry_SetSuppressBrowserIsPersistent covers that the daemon-mode
// switch (unlike the per-call ctx marker) applies to every connect made
// after it is set, not just the next one, and that it can be turned back
// off.
func TestRegistry_SetSuppressBrowserIsPersistent(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	require.False(t, shouldSuppressBrowser(t.Context(), r))

	r.SetSuppressBrowser(true)
	require.True(t, shouldSuppressBrowser(t.Context(), r))
	require.True(t, shouldSuppressBrowser(t.Context(), r), "must stay suppressed across repeated calls, not just the first")

	r.SetSuppressBrowser(false)
	require.False(t, shouldSuppressBrowser(t.Context(), r))
}
