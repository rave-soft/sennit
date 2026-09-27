// This file exercises the client-side half of CLIENT-SERVER.md PR 3.3's
// OAuth relay end to end: StartOAuthCallbackRelay's own contract in
// isolation, then a *Client built with Remote() relaying a browser's GET
// through DeliverOAuthCallback to a stub flow's ServeCallback, over
// bufconn -- no browser and no real network beyond 127.0.0.1 test
// listeners.
package grpcws_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// listenLoopback is net.Listen with a context, satisfying the noctx linter
// every other call site here uses it to keep happy.
func listenLoopback(t *testing.T, addr string) net.Listener {
	t.Helper()
	lc := &net.ListenConfig{}
	lis, err := lc.Listen(t.Context(), "tcp", addr)
	require.NoError(t, err)
	return lis
}

// freeLoopbackAddr binds an ephemeral loopback port, reads it back, and
// releases it immediately -- the small, inherently racy way to hand a
// relay a concrete port to bind next; StartOAuthCallbackRelay binds it
// itself moments later.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	lis := listenLoopback(t, "127.0.0.1:0")
	addr := lis.Addr().String()
	require.NoError(t, lis.Close())
	return addr
}

// httpGet is http.Get with a context, satisfying the noctx linter every
// other call site here uses it to keep happy.
func httpGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestStartOAuthCallbackRelay_ForwardsRequestAndWritesReply checks the
// relay in isolation: a GET against its listener is hand ed to deliver
// with the method/path/query it arrived with, and deliver's reply (status,
// headers, body) is written back to the caller unchanged.
func TestStartOAuthCallbackRelay_ForwardsRequestAndWritesReply(t *testing.T) {
	t.Parallel()

	addr := freeLoopbackAddr(t)
	authURL := fmt.Sprintf("https://example.com/authorize?redirect_uri=%s", "http://"+addr+"/auth/callback")

	var gotMethod, gotPath, gotQuery string
	deliver := func(_ context.Context, method, path, rawQuery string, _ []byte) (int, http.Header, []byte, error) {
		gotMethod, gotPath, gotQuery = method, path, rawQuery
		h := http.Header{}
		h.Set("X-Relayed", "yes")
		return http.StatusOK, h, []byte("ok"), nil
	}

	stop, err := grpcws.StartOAuthCallbackRelay(authURL, deliver)
	require.NoError(t, err)
	defer stop()

	resp := httpGet(t, "http://"+addr+"/auth/callback?code=abc&state=xyz")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.MethodGet, gotMethod)
	require.Equal(t, "/auth/callback", gotPath)
	require.Equal(t, "code=abc&state=xyz", gotQuery)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "yes", resp.Header.Get("X-Relayed"))
	require.Equal(t, "ok", string(body))
}

// TestStartOAuthCallbackRelay_RejectsNonLoopbackHost checks the guard the
// build step called for explicitly: an authorization URL whose
// redirect_uri names a non-loopback host is refused outright, never
// binding a port on every interface for it.
func TestStartOAuthCallbackRelay_RejectsNonLoopbackHost(t *testing.T) {
	t.Parallel()

	_, err := grpcws.StartOAuthCallbackRelay("https://example.com/authorize?redirect_uri=http://evil.example.com:9999/callback", nil)
	require.Error(t, err)
}

// TestStartOAuthCallbackRelay_PortBusy checks a clear, named failure when
// the redirect_uri's port is already taken -- not a silent bind to the
// wrong address.
func TestStartOAuthCallbackRelay_PortBusy(t *testing.T) {
	t.Parallel()

	lis := listenLoopback(t, "127.0.0.1:0")
	defer lis.Close()

	authURL := fmt.Sprintf("https://example.com/authorize?redirect_uri=%s", "http://"+lis.Addr().String()+"/callback")
	_, err := grpcws.StartOAuthCallbackRelay(authURL, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), lis.Addr().String())
}

// TestClientOAuthFlow_StartRelay_CodexEndToEnd is the codex acceptance
// test from CLIENT-SERVER.md PR 3.3's build step: StartOAuth over a
// Remote() Client, then a simulated browser GETs the relay's own listener
// with a code/state pair; the request must reach the (stub) flow's own
// ServeCallback over DeliverOAuthCallback, and the browser must see that
// handler's response.
func TestClientOAuthFlow_StartRelay_CodexEndToEnd(t *testing.T) {
	t.Parallel()

	addr := freeLoopbackAddr(t)
	authURL := fmt.Sprintf("https://auth.example.com/authorize?state=xyz&redirect_uri=%s", "http://"+addr+"/auth/callback")

	served := make(chan *http.Request, 1)
	flow := &wsrpctest.StubOAuthFlow{
		Completion: wsrpctest.OAuthCompletionSample,
		ServeCallbackFn: func(w http.ResponseWriter, r *http.Request) {
			served <- r
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("signed in as a test account"))
		},
	}
	root := &wsrpctest.StubWorkspace{
		OAuthResult: workspace.OAuthStartResult{AuthorizationURL: authURL},
		OAuthFlow:   flow,
	}

	srv, stopHub := grpcws.NewServer(root)
	dialer := startServer(t, srv, stopHub)
	conn, err := dialRawConn(t, dialer)
	require.NoError(t, err)
	client := grpcws.NewClient(conn, grpcws.Remote())

	ctx, cancel := context.WithTimeout(context.Background(), raceWait(10*time.Second))
	defer cancel()

	result, oauthFlow, err := client.StartOAuth(ctx, "codex", "", false)
	require.NoError(t, err)
	require.NotNil(t, oauthFlow)

	stop, err := oauthFlow.StartRelay(ctx, result.AuthorizationURL)
	require.NoError(t, err)
	defer stop()

	// Play the browser: GET the relay's listener with a code and state,
	// exactly as the authorization server's redirect would.
	resp := httpGet(t, "http://"+addr+"/auth/callback?code=abc123&state=xyz")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "signed in as a test account", string(body))

	select {
	case r := <-served:
		require.Equal(t, "/auth/callback", r.URL.Path)
		require.Equal(t, "abc123", r.URL.Query().Get("code"))
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("the flow's ServeCallback was never invoked")
	}

	completion, err := oauthFlow.Wait(ctx)
	require.NoError(t, err)
	require.Equal(t, wsrpctest.OAuthCompletionSample, completion)
}

// TestClientOAuthFlow_StartRelay_WrongPathRejected checks that a request
// for anything but the flow's own callback path is rejected by the flow's
// handler (never turned into a 500 by the relay itself), matching the red
// check the build step calls for on a mismatch.
func TestClientOAuthFlow_StartRelay_WrongPathRejected(t *testing.T) {
	t.Parallel()

	addr := freeLoopbackAddr(t)
	authURL := fmt.Sprintf("https://auth.example.com/authorize?redirect_uri=%s", "http://"+addr+"/auth/callback")

	flow := &wsrpctest.StubOAuthFlow{
		ServeCallbackFn: func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		},
	}
	root := &wsrpctest.StubWorkspace{
		OAuthResult: workspace.OAuthStartResult{AuthorizationURL: authURL},
		OAuthFlow:   flow,
	}

	srv, stopHub := grpcws.NewServer(root)
	dialer := startServer(t, srv, stopHub)
	conn, err := dialRawConn(t, dialer)
	require.NoError(t, err)
	client := grpcws.NewClient(conn, grpcws.Remote())

	ctx, cancel := context.WithTimeout(context.Background(), raceWait(10*time.Second))
	defer cancel()

	result, oauthFlow, err := client.StartOAuth(ctx, "codex", "", false)
	require.NoError(t, err)
	stop, err := oauthFlow.StartRelay(ctx, result.AuthorizationURL)
	require.NoError(t, err)
	defer stop()

	resp := httpGet(t, "http://"+addr+"/favicon.ico")
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestClientOAuthFlow_StartRelay_LocalNoOp checks the local-daemon/
// in-process contract: a Client built WITHOUT Remote() hands back a
// StartRelay that succeeds without binding anything -- the redirect_uri's
// port stays free, since the daemon's own listener (same machine here)
// already owns it.
func TestClientOAuthFlow_StartRelay_LocalNoOp(t *testing.T) {
	t.Parallel()

	addr := freeLoopbackAddr(t)
	authURL := fmt.Sprintf("https://auth.example.com/authorize?redirect_uri=%s", "http://"+addr+"/auth/callback")

	root := &wsrpctest.StubWorkspace{
		OAuthResult: workspace.OAuthStartResult{AuthorizationURL: authURL},
		OAuthFlow:   &wsrpctest.StubOAuthFlow{},
	}
	client := newServerAndClient(t, root)

	ctx, cancel := context.WithTimeout(context.Background(), raceWait(10*time.Second))
	defer cancel()

	result, oauthFlow, err := client.StartOAuth(ctx, "codex", "", false)
	require.NoError(t, err)
	stop, err := oauthFlow.StartRelay(ctx, result.AuthorizationURL)
	require.NoError(t, err)
	defer stop()

	// The relay must not have bound the redirect_uri's port: binding it
	// ourselves right now must still succeed.
	lis := listenLoopback(t, addr)
	require.NoError(t, lis.Close())
}
