// This file exercises StartOAuth/OAuthWait/OAuthCancel over real gRPC
// (bufconn), the CLIENT-SERVER.md PR 1.3b-2 acceptance tests: a redirect-
// style and a device-style flow both round-trip their result and
// completion fields, a reused login is reported with no flow at all, a
// Wait error keeps its identity through the trailer, cancelling the
// caller's ctx cancels the wait server-side and Cancel then reaches the
// stub exactly once, and a client that goes quiet mid sign-in (a half-open
// connection, so only keepalive notices) has its pending flow cancelled
// and dropped by the same lease sweep that releases worktree/thread
// handles.
package grpcws_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/wsrpctest"
)

// TestStartOAuth_RedirectFlow_WaitReturnsCompletion checks a redirect-style
// flow (an AuthorizationURL, no device code) end to end: StartOAuth's
// result round-trips, Wait returns the completion the stub was primed
// with, and the server has dropped the flow's entry afterward (a second
// Wait on the same handle answers ErrWorkspaceGone).
func TestStartOAuth_RedirectFlow_WaitReturnsCompletion(t *testing.T) {
	t.Parallel()

	wantResult := workspace.OAuthStartResult{AuthorizationURL: "https://example.com/authorize?state=abc", ExpiresIn: 600}
	flow := &wsrpctest.StubOAuthFlow{Completion: wsrpctest.OAuthCompletionSample}
	root := &wsrpctest.StubWorkspace{OAuthResult: wantResult, OAuthFlow: flow}
	client := newServerAndClient(t, root)

	gotResult, gotFlow, err := client.StartOAuth(context.Background(), "codex", "", false)
	require.NoError(t, err)
	require.Equal(t, "codex", root.GotOAuthProviderID)
	require.Equal(t, wantResult, gotResult)
	require.NotNil(t, gotFlow)

	completion, err := gotFlow.Wait(context.Background())
	require.NoError(t, err)
	require.Equal(t, wsrpctest.OAuthCompletionSample, completion)

	// The server dropped the flow's entry the moment Wait returned; a
	// second Wait on the same handle finds nothing left to wait on.
	_, err = gotFlow.Wait(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrWorkspaceGone), "expected ErrWorkspaceGone once the flow's entry is gone, got: %v", err)
}

// TestStartOAuth_DeviceFlow_WaitReturnsCompletion is the redirect test's
// device-style sibling (UserCode/VerificationURL, no AuthorizationURL).
func TestStartOAuth_DeviceFlow_WaitReturnsCompletion(t *testing.T) {
	t.Parallel()

	wantResult := workspace.OAuthStartResult{
		DeviceCode: "dev-1", UserCode: "ABCD-1234", VerificationURL: "https://example.com/device", Interval: 5, ExpiresIn: 900,
	}
	flow := &wsrpctest.StubOAuthFlow{Completion: wsrpctest.OAuthCompletionSample}
	root := &wsrpctest.StubWorkspace{OAuthResult: wantResult, OAuthFlow: flow}
	client := newServerAndClient(t, root)

	gotResult, gotFlow, err := client.StartOAuth(context.Background(), "copilot", "", true)
	require.NoError(t, err)
	require.Equal(t, "copilot", root.GotOAuthProviderID)
	require.Equal(t, wantResult, gotResult)
	require.NotNil(t, gotFlow)

	completion, err := gotFlow.Wait(context.Background())
	require.NoError(t, err)
	require.Equal(t, wsrpctest.OAuthCompletionSample, completion)
}

// TestStartOAuth_ReusedLogin_NoFlowHandle checks
// workspace.OAuthStartResult's own contract survives the wire: when the
// server reused an existing login (Completed set), no flow handle is
// minted at all, and the client hands back a nil OAuthFlow rather than one
// that would just fail on first use.
func TestStartOAuth_ReusedLogin_NoFlowHandle(t *testing.T) {
	t.Parallel()

	completion := wsrpctest.OAuthCompletionSample
	wantResult := workspace.OAuthStartResult{ReusedExistingLogin: true, Completed: &completion}
	root := &wsrpctest.StubWorkspace{OAuthResult: wantResult}
	client := newServerAndClient(t, root)

	gotResult, gotFlow, err := client.StartOAuth(context.Background(), "codex", "", false)
	require.NoError(t, err)
	require.Equal(t, wantResult, gotResult)
	require.Nil(t, gotFlow, "a reused login must hand back no OAuthFlow at all")
}

// TestOAuthWait_ErrorKeepsIdentityThroughTrailer checks that flow.Wait's
// own error is not just a generic failure once it crosses the wire: a
// sentinel error decodes back with the same identity (errors.Is), the same
// contract every other Workspace method's error already has (see
// decodeClientError's doc comment).
func TestOAuthWait_ErrorKeepsIdentityThroughTrailer(t *testing.T) {
	t.Parallel()

	flow := &wsrpctest.StubOAuthFlow{WaitErr: workspace.ErrThreadsNotSupported}
	root := &wsrpctest.StubWorkspace{OAuthFlow: flow}
	client := newServerAndClient(t, root)

	_, gotFlow, err := client.StartOAuth(context.Background(), "codex", "", false)
	require.NoError(t, err)

	_, err = gotFlow.Wait(context.Background())
	require.Error(t, err)
	require.True(t, errors.Is(err, workspace.ErrThreadsNotSupported), "expected the sentinel's identity preserved, got: %v", err)
}

// TestOAuthWait_ClientCtxCancelCancelsServerWait checks OAuthWait's central
// contract (CLIENT-SERVER.md, PR 1.3b-2): unlike AgentRunStream's detached
// turnCtx, the client's own ctx cancellation must reach flow.Wait
// server-side -- the sign-in belongs to the person waiting on it. Once
// Wait returns (here, because its ctx ended), the flow's entry is dropped
// and Cancel has already run exactly once; a client that then also calls
// Cancel (mirroring internal/cmd/login.go's unconditional defer
// flow.Cancel()) must not run it a second time.
func TestOAuthWait_ClientCtxCancelCancelsServerWait(t *testing.T) {
	t.Parallel()

	flow := &wsrpctest.StubOAuthFlow{
		WaitCtxCh: make(chan context.Context, 1),
		WaitDone:  make(chan struct{}), // never closed -- Wait only returns via ctx.
	}
	root := &wsrpctest.StubWorkspace{OAuthFlow: flow}
	client := newServerAndClient(t, root)

	_, gotFlow, err := client.StartOAuth(context.Background(), "codex", "", false)
	require.NoError(t, err)

	waitCtx, cancel := context.WithCancel(context.Background())
	waitErrCh := make(chan error, 1)
	go func() {
		_, err := gotFlow.Wait(waitCtx)
		waitErrCh <- err
	}()

	var serverCtx context.Context
	select {
	case serverCtx = <-flow.WaitCtxCh:
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("flow.Wait was never called server-side")
	}

	cancel()

	select {
	case err := <-waitErrCh:
		require.Error(t, err)
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("Wait never returned after the caller's ctx was cancelled")
	}

	select {
	case <-serverCtx.Done():
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("the server-side ctx passed to flow.Wait was never cancelled")
	}

	waitFor(t, raceWait(5*time.Second), func() bool {
		flow.CancelMu.Lock()
		defer flow.CancelMu.Unlock()
		return flow.CancelCalls == 1
	})

	// The contract every caller in this tree follows: Cancel is always
	// called exactly once, even after Wait has already returned. It must
	// be harmless here, since OAuthWait's own handler already dropped the
	// entry and ran Cancel once.
	gotFlow.Cancel()
	flow.CancelMu.Lock()
	got := flow.CancelCalls
	flow.CancelMu.Unlock()
	require.Equal(t, 1, got, "a Cancel call after Wait has already returned must not run flow.Cancel a second time")
}

// TestLeaseGrace_CancelsPendingOAuthFlowOnHalfOpenDisconnect is the OAuth
// counterpart of handles_wire_test.go's
// TestLeaseGrace_ReleasesHandlesOnceClientGoesQuiet_RealDisconnectNoRedial:
// a client starts a sign-in (StartOAuth) but never opens OAuthWait, then
// its connection goes half-open -- the client never signals anything,
// ever, and never dials again. Only the server's own keepalive can notice
// this at all; once it does, the lease sweep must cancel and drop the
// still-pending flow the same way it releases worktree/thread handles, so
// a vanished frontend never leaves a loopback callback listener bound
// (CLIENT-SERVER.md's build note on internal/oauth/codex/oauth.go's fixed
// callback port).
func TestLeaseGrace_CancelsPendingOAuthFlowOnHalfOpenDisconnect(t *testing.T) {
	t.Parallel()

	pingTime, pingTimeout, grace := keepaliveTuning()
	flow := &wsrpctest.StubOAuthFlow{WaitDone: make(chan struct{})}
	root := &wsrpctest.StubWorkspace{OAuthFlow: flow, SubscribeWithReady: make(chan struct{})}
	srv, stopHub := grpcws.NewServer(root,
		grpcws.WithKeepaliveParams(pingTime, pingTimeout),
		grpcws.WithHandleLeaseGrace(grace),
	)
	lis := bufconn.Listen(bufSize)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		stopHub()
		_ = lis.Close()
	})

	// No client-side keepalive: see
	// TestKeepalive_HalfOpenConnection_ReleasesHandlesAndTurnSurvives's
	// doc comment on why one would let this test pass even with the
	// server-side fix reverted.
	dialer := &halfOpenDialer{lis: lis}
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer.dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := grpcws.NewClient(conn)

	// An open Subscribe stream, exactly like the handles test, so the
	// connection reads as "present" until keepalive says otherwise --
	// nothing else here would ever notice a half-open connection at all.
	stopSub := client.SubscribeWith(func(any) {})
	t.Cleanup(stopSub)
	select {
	case <-root.SubscribeWithReady:
	case <-time.After(raceWait(5 * time.Second)):
		t.Fatal("Subscribe never reached the server")
	}

	_, _, err = client.StartOAuth(context.Background(), "codex", "", false)
	require.NoError(t, err)

	dialer.cut()

	waitFor(t, pingTime+pingTimeout+grace+10*time.Second, func() bool {
		flow.CancelMu.Lock()
		defer flow.CancelMu.Unlock()
		return flow.CancelCalls == 1
	})
}

// TestNewServer_OAuthFlowNoGoroutineLeak mirrors
// TestNewServer_NonRootHandleNoGoroutineLeak: a pending flow that is never
// waited on or cancelled by the client must not survive the server
// stopping -- NewServer's stop func's own oauthRegistry.closeAll (see
// server.go) is what this exercises.
func TestNewServer_OAuthFlowNoGoroutineLeak(t *testing.T) {
	ignoreBaseline := goleak.IgnoreCurrent()

	func() {
		flow := &wsrpctest.StubOAuthFlow{WaitDone: make(chan struct{})}
		root := &wsrpctest.StubWorkspace{OAuthFlow: flow}
		srv, stopHub := grpcws.NewServer(root)
		lis := bufconn.Listen(bufSize)
		go func() { _ = srv.Serve(lis) }()

		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		require.NoError(t, err)
		client := grpcws.NewClient(conn)

		_, _, err = client.StartOAuth(context.Background(), "codex", "", false)
		require.NoError(t, err)

		client.Shutdown()
		require.NoError(t, conn.Close())
		srv.Stop()
		stopHub()
		require.NoError(t, lis.Close())
	}()

	waitFor(t, raceWait(5*time.Second), func() bool {
		return goleak.Find(ignoreBaseline) == nil
	})
}
