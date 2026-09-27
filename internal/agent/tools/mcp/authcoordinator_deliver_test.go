package mcp

// This file exercises authCoordinator.DeliverAuthCallback (CLIENT-
// SERVER.md, PR 3.3): the daemon-side hand-off from
// grpcws.oauthServer.DeliverOAuthCallback to a named MCP server's own
// mcpoauth.Handler, driven with real net/http requests/recorders rather
// than a live authorization server.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	mcpoauth "github.com/rave-soft/sennit/internal/oauth/mcp"
)

// TestAuthCoordinator_DeliverAuthCallback_UnknownServerReportsFalse checks
// the "nothing pending for this name" case, which grpcws.oauthServer turns
// into a 404 for the browser rather than an RPC error.
func TestAuthCoordinator_DeliverAuthCallback_UnknownServerReportsFalse(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?code=1&state=2", nil)

	require.False(t, r.DeliverAuthCallback("no-such-server", rec, req))
}

// TestAuthCoordinator_DeliverAuthCallback_PublishedHandlerServesIt checks
// the found case: a server currently publishing a handler (as BeginAuth's
// worker does via authURLs.Set) has its callback served exactly as its own
// loopback listener would -- the wrong path 404s, the right one renders
// the callback page.
func TestAuthCoordinator_DeliverAuthCallback_PublishedHandlerServesIt(t *testing.T) {
	t.Parallel()

	const name = "test-server"
	handler, err := mcpoauth.NewHandler(name, "https://example.test", nil, nil, nil, true, 0)
	require.NoError(t, err)
	t.Cleanup(handler.Close)

	r := NewRegistry()
	r.publishMu.Lock()
	r.authURLs.Set(name, authPublication{auth: newOwnedAuthHandler(handler)})
	r.publishMu.Unlock()

	// The wrong path is rejected by the handler's own check, not silently
	// accepted -- mirrors the codex flow's identical guard.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/wrong-path", nil)
	require.True(t, r.DeliverAuthCallback(name, rec, req))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// The real callback path renders the callback page, exactly as the
	// handler's own listener would for a redirect with no flight
	// currently in progress.
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/callback?code=abc&state=xyz", nil)
	require.True(t, r.DeliverAuthCallback(name, rec, req))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, rec.Body.String())
}
