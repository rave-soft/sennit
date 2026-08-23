package mcp

import (
	"net/http"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// idleCloser records whether its CloseIdleConnections was called, standing
// in for the http.Transport an HTTP-based MCP connection keeps alive.
type idleCloser struct {
	http.RoundTripper
	closed bool
}

func (c *idleCloser) CloseIdleConnections() { c.closed = true }

// TestCloseIdleTransportSeesThroughTheChannelWrapper pins the leak: every
// connection is wrapped in a channelTransport before the SDK gets it, and
// closeIdleTransport type-switched on the wrapper — so it always answered
// "nothing to close" and the keep-alive connections, along with their
// http.Transport goroutines, survived every renew, teardown and Close.
func TestCloseIdleTransportSeesThroughTheChannelWrapper(t *testing.T) {
	t.Parallel()

	rt := &idleCloser{RoundTripper: http.DefaultTransport}
	inner := &mcp.StreamableClientTransport{
		Endpoint:   "http://127.0.0.1:9/mcp",
		HTTPClient: &http.Client{Transport: rt},
	}
	wrapped := &channelTransport{inner: inner, name: "srv", gate: newChannelGate()}

	closeIdle := closeIdleTransport(wrapped)
	require.NotNil(t, closeIdle, "the wrapper must not hide an HTTP transport's idle connections")

	closeIdle()
	require.True(t, rt.closed)
}

// TestUnwrapTransportReachesTheCommandTransport is the stdio half: the
// npx-cannot-start diagnostic maybeStdioErr exists for only runs when the
// transport is recognised as a CommandTransport.
func TestUnwrapTransportReachesTheCommandTransport(t *testing.T) {
	t.Parallel()

	inner := &mcp.CommandTransport{Command: exec.Command("true")}
	wrapped := &channelTransport{inner: inner, name: "srv", gate: newChannelGate()}

	got, ok := unwrapTransport(wrapped).(*mcp.CommandTransport)
	require.True(t, ok, "a wrapped command transport must still be recognisable")
	require.Same(t, inner, got)
}

// TestUnwrapTransportLeavesAnUnwrappedTransportAlone keeps the seam from
// changing anything for a transport nothing wrapped.
func TestUnwrapTransportLeavesAnUnwrappedTransportAlone(t *testing.T) {
	t.Parallel()

	inner := &mcp.CommandTransport{Command: exec.Command("true")}
	require.Same(t, mcp.Transport(inner), unwrapTransport(inner))
}
