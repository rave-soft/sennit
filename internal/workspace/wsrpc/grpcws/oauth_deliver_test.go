package grpcws

// This file exercises DeliverOAuthCallback (CLIENT-SERVER.md, PR 3.3)
// directly against oauthServer/oauthFlowRegistry, underneath the wire --
// oauth_relay_test.go (grpcws_test) exercises the same RPC end to end,
// including the client-side relay, over bufconn.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/rave-soft/sennit/internal/workspace"
)

// httpCallbackStubFlow is a workspace.OAuthFlow that also implements
// httpCallbackFlow, so DeliverOAuthCallback's "codex" path has something
// to type-assert against and invoke.
type httpCallbackStubFlow struct {
	stubFlow
	serve func(w http.ResponseWriter, r *http.Request)
}

func (f *httpCallbackStubFlow) ServeCallback(w http.ResponseWriter, r *http.Request) {
	f.serve(w, r)
}

func withClientID(id string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(clientMetadataKey, id))
}

// TestOAuthFlowRegistry_ResolveOwned checks resolveOwned's own contract:
// it finds a flow only for the owner it was registered under, and reports
// false for a wrong owner, an empty owner claiming someone else's flow, or
// an unknown handle -- never panicking or leaking which of those it was.
func TestOAuthFlowRegistry_ResolveOwned(t *testing.T) {
	t.Parallel()

	r := newOAuthFlowRegistry()
	flow := &stubFlow{}
	handle := r.register(flow, "owner-a")

	got, ok := r.resolveOwned(handle, "owner-a")
	require.True(t, ok)
	require.Same(t, workspace.OAuthFlow(flow), got)

	_, ok = r.resolveOwned(handle, "owner-b")
	require.False(t, ok, "a different owner must not resolve someone else's flow")

	_, ok = r.resolveOwned(handle, "")
	require.False(t, ok, "an empty owner must not resolve a flow registered to a real owner")

	_, ok = r.resolveOwned("never-registered", "owner-a")
	require.False(t, ok)
}

// TestOAuthServer_DeliverOAuthCallback_Codex checks the "codex" path: a
// flow's ServeCallback is invoked with a synthesized request built from
// the RPC's Method/Path/RawQuery/Body, and its HTTP response is carried
// back verbatim.
func TestOAuthServer_DeliverOAuthCallback_Codex(t *testing.T) {
	t.Parallel()

	registry := newOAuthFlowRegistry()
	var gotPath, gotQuery string
	flow := &httpCallbackStubFlow{serve: func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("signed in"))
	}}
	handle := registry.register(flow, "owner-a")

	s := &oauthServer{registry: registry}
	resp, err := s.DeliverOAuthCallback(withClientID("owner-a"), &DeliverOAuthCallbackRequest{
		Kind: "codex", FlowHandle: handle, Method: http.MethodGet,
		Path: "/auth/callback", RawQuery: "code=abc&state=xyz",
	})
	require.NoError(t, err)
	require.Equal(t, "/auth/callback", gotPath)
	require.Equal(t, "code=abc&state=xyz", gotQuery)
	require.Equal(t, http.StatusOK, resp.Status)
	require.Equal(t, []string{"yes"}, resp.Headers["X-Test"])
	require.Equal(t, "signed in", string(resp.Body))
}

// TestOAuthServer_DeliverOAuthCallback_Codex_WrongOwnerRejected checks the
// ownership check the build step called for explicitly: a caller whose
// lease didn't mint the handle gets nothing back.
func TestOAuthServer_DeliverOAuthCallback_Codex_WrongOwnerRejected(t *testing.T) {
	t.Parallel()

	registry := newOAuthFlowRegistry()
	called := false
	flow := &httpCallbackStubFlow{serve: func(http.ResponseWriter, *http.Request) { called = true }}
	handle := registry.register(flow, "owner-a")

	s := &oauthServer{registry: registry}
	_, err := s.DeliverOAuthCallback(withClientID("owner-b"), &DeliverOAuthCallbackRequest{
		Kind: "codex", FlowHandle: handle, Method: http.MethodGet, Path: "/auth/callback",
	})
	require.Error(t, err)
	require.False(t, called, "a rejected delivery must never reach the flow's own handler")
}

// TestOAuthServer_DeliverOAuthCallback_UnknownKindRejected checks the
// default arm: an unrecognized Kind is refused outright rather than
// falling through to either registry.
func TestOAuthServer_DeliverOAuthCallback_UnknownKindRejected(t *testing.T) {
	t.Parallel()

	s := &oauthServer{registry: newOAuthFlowRegistry()}
	_, err := s.DeliverOAuthCallback(context.Background(), &DeliverOAuthCallbackRequest{Kind: "bogus"})
	require.Error(t, err)
}

// mcpDeliverStubWorkspace implements mcpCallbackDeliverer structurally,
// underneath the Workspace resolve func oauthServer.resolve returns.
type mcpDeliverStubWorkspace struct {
	workspace.Workspace
	deliver func(name string, w http.ResponseWriter, r *http.Request) bool
}

func (s *mcpDeliverStubWorkspace) DeliverMCPOAuthCallback(name string, w http.ResponseWriter, r *http.Request) bool {
	return s.deliver(name, w, r)
}

// TestOAuthServer_DeliverOAuthCallback_MCP checks the "mcp" path: it
// resolves the root workspace (not the flow registry) and hands it
// ServerName rather than a flow handle -- no per-client ownership check,
// per authCoordinator.DeliverAuthCallback's own doc comment.
func TestOAuthServer_DeliverOAuthCallback_MCP(t *testing.T) {
	t.Parallel()

	var gotName string
	ws := &mcpDeliverStubWorkspace{deliver: func(name string, w http.ResponseWriter, r *http.Request) bool {
		gotName = name
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mcp authorized"))
		return true
	}}
	s := &oauthServer{resolve: func(context.Context) (workspace.Workspace, error) { return ws, nil }}

	resp, err := s.DeliverOAuthCallback(context.Background(), &DeliverOAuthCallbackRequest{
		Kind: "mcp", ServerName: "my-server", Method: http.MethodGet, Path: "/callback", RawQuery: "code=1&state=2",
	})
	require.NoError(t, err)
	require.Equal(t, "my-server", gotName)
	require.Equal(t, http.StatusOK, resp.Status)
	require.Equal(t, "mcp authorized", string(resp.Body))
}

// TestOAuthServer_DeliverOAuthCallback_MCP_NotFoundBecomes404 checks the
// "no such pending server" case: DeliverMCPOAuthCallback reporting false
// (server unknown, or nothing pending for it) surfaces as an ordinary 404
// response rather than an RPC error -- the browser gets a page instead of
// a broken connection.
func TestOAuthServer_DeliverOAuthCallback_MCP_NotFoundBecomes404(t *testing.T) {
	t.Parallel()

	ws := &mcpDeliverStubWorkspace{deliver: func(string, http.ResponseWriter, *http.Request) bool { return false }}
	s := &oauthServer{resolve: func(context.Context) (workspace.Workspace, error) { return ws, nil }}

	resp, err := s.DeliverOAuthCallback(context.Background(), &DeliverOAuthCallbackRequest{
		Kind: "mcp", ServerName: "unknown", Method: http.MethodGet, Path: "/callback",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.Status)
}

// TestOAuthServer_DeliverOAuthCallback_MCP_UnsupportedWorkspaceRejected
// checks a workspace with no MCP delivery capability at all (a read-only
// wrapper, most test stubs) fails outright rather than silently no-op-ing.
func TestOAuthServer_DeliverOAuthCallback_MCP_UnsupportedWorkspaceRejected(t *testing.T) {
	t.Parallel()

	s := &oauthServer{resolve: func(context.Context) (workspace.Workspace, error) { return nil, nil }}
	_, err := s.DeliverOAuthCallback(context.Background(), &DeliverOAuthCallbackRequest{Kind: "mcp", ServerName: "x"})
	require.Error(t, err)
}
