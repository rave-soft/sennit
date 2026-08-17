package codex

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// testCallbackPort is what a flow binds during this package's tests. Fixed,
// like the real one — the tests here are sequential, so they never race each
// other for it — but not the real one, and outside the ephemeral range the
// rest of the suite's httptest servers draw from.
const testCallbackPort = 14455

// TestMain moves the sign-in callback port off the one real port for the
// whole package.
//
// Several tests here start a flow, and a flow binds a single fixed port
// because that is the only redirect URI the authorization server accepts.
// That port is global to the machine: `go test ./...` builds and runs
// package test binaries concurrently, a developer may have a real sign-in
// (Sennit's or the Codex CLI's) open while running them, and any of that
// turns "bind the callback port" into a coin flip. It failed exactly that
// way in CI. Only the port moves — everything each test asserts about the
// flow is unchanged.
func TestMain(m *testing.M) {
	callbackPort = testCallbackPort
	os.Exit(m.Run())
}

// startTestFlow starts a flow, or skips the test if the port is held by
// something outside this test binary. Whatever holds it, the test cannot
// prove anything about a listener it was not allowed to open, and failing
// the build over a machine-wide resource is the wrong verdict.
func startTestFlow(t *testing.T, proxyURL string) *Flow {
	t.Helper()
	flow, err := StartFlow(proxyURL)
	if errors.Is(err, syscall.EADDRINUSE) {
		t.Skipf("callback port %d is already in use", callbackPort)
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Close() })
	return flow
}

// TestRedirectURIIsTheOneTheClientAccepts pins the real value that TestMain
// moves out of the way. The OAuth client is registered against this exact
// string: a change here is a change to something the authorization server
// enforces, not a local detail.
func TestRedirectURIIsTheOneTheClientAccepts(t *testing.T) {
	t.Parallel()

	require.Equal(t, 1455, defaultCallbackPort)
	require.Equal(t, "/auth/callback", callbackPath)
	require.Equal(t, "http://localhost:1455/auth/callback",
		fmt.Sprintf("http://localhost:%d%s", defaultCallbackPort, callbackPath))
}
