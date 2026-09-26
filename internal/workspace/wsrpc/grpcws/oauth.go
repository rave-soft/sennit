package grpcws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/workspace"
)

// oauthServiceName is the fifth hand-written service (alongside Meta,
// Events, Agent and Handles): StartOAuth is class H, like
// EnterWorktree/ExitWorktree/AttachThread, but its handle is a
// workspace.OAuthFlow, not another workspace.Workspace, so it doesn't fit
// the Handles service's shape -- OAuthWait/OAuthCancel act on that flow
// handle directly, never through "sennit-handle" metadata (CLIENT-SERVER.md,
// PR 1.3b-2).
const oauthServiceName = "sennit.workspace.v1.OAuth"

// StartOAuthRequest is StartOAuth's request.
type StartOAuthRequest struct {
	ProviderID      string `json:"provider_id"`
	ProxyURL        string `json:"proxy_url"`
	ForceNewAccount bool   `json:"force_new_account"`
}

// StartOAuthResponse is what StartOAuth hands back: Result is always set;
// FlowHandle names the flow OAuthWait/OAuthCancel act on, and is empty
// exactly when Result.Completed is set (an existing login was reused, so
// there is nothing left to wait on) -- matching
// workspace.OAuthStartResult's own doc comment that the accompanying
// OAuthFlow is nil then.
type StartOAuthResponse struct {
	Result     workspace.OAuthStartResult `json:"result"`
	FlowHandle string                     `json:"flow_handle,omitempty"`
}

// OAuthWaitRequest is OAuthWait's request.
type OAuthWaitRequest struct {
	FlowHandle string `json:"flow_handle"`
}

// OAuthWaitFrame is OAuthWait's response stream's one and only message on
// success -- a server-stream rather than a unary RPC only so the client's
// own ctx cancels the wait server-side (see OAuthServer.OAuthWait's doc
// comment): a unary call's ctx can only ever be observed by the RPC
// itself, not turned into the stream cancellation flow.Wait needs. A
// failure is carried as the RPC's own error (trailer-encoded, identity-
// preserving -- see grpcStatusFromError), matching AgentRunShellCommand's
// convention instead of an Err field here.
type OAuthWaitFrame struct {
	Completion workspace.OAuthCompletion `json:"completion"`
}

// OAuthCancelRequest is OAuthCancel's request.
type OAuthCancelRequest struct {
	FlowHandle string `json:"flow_handle"`
}

// OAuthCancelResponse is OAuthCancel's (empty) response.
type OAuthCancelResponse struct{}

// OAuthServer is the interface grpc.Server.RegisterService checks the
// registered handler against (see MetaServer's doc comment for why this
// can't just be *oauthServer).
type OAuthServer interface {
	StartOAuth(context.Context, *StartOAuthRequest) (*StartOAuthResponse, error)
	OAuthCancel(context.Context, *OAuthCancelRequest) (*OAuthCancelResponse, error)
	OAuthWait(*OAuthWaitRequest, OAuthWaitServer) error
}

// OAuthWaitServer is the server side of the OAuthWait stream.
type OAuthWaitServer interface {
	Send(*OAuthWaitFrame) error
	grpc.ServerStream
}

type oauthWaitServer struct {
	grpc.ServerStream
}

func (x *oauthWaitServer) Send(m *OAuthWaitFrame) error {
	return x.SendMsg(m)
}

// oauthUnaryHandler builds a grpc.MethodDesc.Handler for one OAuthServer
// unary method, following handlesUnaryHandler's own shape (see its doc
// comment) -- kept generic rather than two hand-copied bodies for the same
// reason that one is.
func oauthUnaryHandler[Req, Resp any](methodName string, call func(*oauthServer, context.Context, *Req) (*Resp, error)) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(Req)
		if err := dec(in); err != nil {
			return nil, err
		}
		s := srv.(*oauthServer)
		if interceptor == nil {
			return call(s, ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + oauthServiceName + "/" + methodName}
		handler := func(ctx context.Context, req any) (any, error) {
			return call(s, ctx, req.(*Req))
		}
		return interceptor(ctx, in, info, handler)
	}
}

var _OAuth_StartOAuth_Handler = oauthUnaryHandler("StartOAuth", (*oauthServer).StartOAuth)

var _OAuth_OAuthCancel_Handler = oauthUnaryHandler("OAuthCancel", (*oauthServer).OAuthCancel)

func _OAuth_OAuthWait_Handler(srv any, stream grpc.ServerStream) error {
	m := new(OAuthWaitRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(OAuthServer).OAuthWait(m, &oauthWaitServer{stream})
}

var oauthServiceDesc = grpc.ServiceDesc{
	ServiceName: oauthServiceName,
	HandlerType: (*OAuthServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "StartOAuth", Handler: _OAuth_StartOAuth_Handler},
		{MethodName: "OAuthCancel", Handler: _OAuth_OAuthCancel_Handler},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "OAuthWait",
			Handler:       _OAuth_OAuthWait_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "wsrpc/oauth",
}

// oauthServer adapts a resolved workspace.Workspace and an oauthFlowRegistry
// to OAuthServer. resolve mirrors handlesServer's own: StartOAuth acts on
// whatever workspace this client is currently viewing, the same as any
// other Workspace method, not on the flow it's about to mint.
type oauthServer struct {
	resolve  func(ctx context.Context) (workspace.Workspace, error)
	registry *oauthFlowRegistry
}

// StartOAuth implements the RPC from CLIENT-SERVER.md, PR 1.3b-2's build
// step 1. A flow StartOAuth returns is registered under a fresh handle,
// owned by this call's client lease (clientIDFromContext), the same
// ownership handleRegistry.register uses -- leaseManager.expire sweeps
// both registries for the same owner (see server.go's NewServer).
func (s *oauthServer) StartOAuth(ctx context.Context, req *StartOAuthRequest) (*StartOAuthResponse, error) {
	ws, err := s.resolve(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	result, flow, startErr := ws.StartOAuth(ctx, req.ProviderID, req.ProxyURL, req.ForceNewAccount)
	if startErr != nil {
		return nil, grpcStatusFromError(ctx, startErr)
	}
	resp := &StartOAuthResponse{Result: result}
	if flow != nil {
		resp.FlowHandle = s.registry.register(flow, clientIDFromContext(ctx))
	}
	return resp, nil
}

// OAuthCancel drops req.FlowHandle from the registry, running its flow's
// Cancel exactly once -- see oauthFlowRegistry.release's doc comment on why
// an unknown handle (already gone via OAuthWait's own drop, or a lease
// sweep, or simply called twice) is not an error.
func (s *oauthServer) OAuthCancel(_ context.Context, req *OAuthCancelRequest) (*OAuthCancelResponse, error) {
	s.registry.release(req.FlowHandle)
	return &OAuthCancelResponse{}, nil
}

// OAuthWait implements the RPC from CLIENT-SERVER.md, PR 1.3b-2's build
// step 1: unlike AgentRunStream's turnCtx, stream.Context() is passed
// straight through to flow.Wait -- the sign-in belongs to the person
// waiting on it (this method's own doc comment on OAuthWaitFrame), so
// cancelling the client's ctx, or the connection simply dying, must cancel
// the wait itself rather than merely stop observing it. Either way, once
// Wait returns, this flow's entry is dropped and its Cancel is run exactly
// once (registry.release) before this method returns -- so a client that
// also calls OAuthCancel afterward, per the "Cancel is always called
// exactly once by callers" contract every caller in this tree already
// follows (see internal/cmd/login.go's defer flow.Cancel(), right after
// StartOAuth, unconditionally), finds nothing left to cancel: exactly the
// no-op OAuthCancel's own doc comment promises.
func (s *oauthServer) OAuthWait(req *OAuthWaitRequest, stream OAuthWaitServer) (err error) {
	// grpc-go does not recover a streaming handler's own panics -- see
	// agentServer.AgentRunStream's identical guard.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Wsrpc OAuthWait stream panicked, ending only this stream", "panic", r)
			err = status.Errorf(codes.Internal, "wsrpc: internal error")
		}
	}()

	ctx := stream.Context()
	flow, ok := s.registry.resolve(req.FlowHandle)
	if !ok {
		return grpcStatusFromError(ctx, workspace.ErrWorkspaceGone)
	}
	completion, waitErr := flow.Wait(ctx)
	s.registry.release(req.FlowHandle)
	if waitErr != nil {
		return grpcStatusFromError(ctx, waitErr)
	}
	return stream.Send(&OAuthWaitFrame{Completion: completion})
}

// oauthFlowEntry is one pending OAuth sign-in flow StartOAuth minted,
// awaiting OAuthWait or OAuthCancel.
type oauthFlowEntry struct {
	flow  workspace.OAuthFlow
	owner string
	once  sync.Once
}

// oauthFlowRegistry maps flow handles to the workspace.OAuthFlow StartOAuth
// returned. It mirrors handleRegistry's owner-based lease and idempotent
// release (CLIENT-SERVER.md, PR 1.3), but for OAuthFlow rather than
// workspace.Workspace: the two aren't merged into one map because an
// OAuthFlow carries no event hub and resolves through a different service
// (this file's oauthServer, not HandlesServer) -- reusing handleRegistry's
// own type would mean every resolve/resolveHub caller now has to handle a
// nil hub for an OAuth entry. What genuinely is shared is leaseManager
// itself: expire sweeps both registries for the same owner, so a client
// that goes quiet loses its worktree/thread handles and its pending
// sign-ins together, in one place.
type oauthFlowRegistry struct {
	mu   sync.Mutex
	byID map[string]*oauthFlowEntry
}

func newOAuthFlowRegistry() *oauthFlowRegistry {
	return &oauthFlowRegistry{byID: map[string]*oauthFlowEntry{}}
}

// register mints a fresh, unguessable handle for flow (see randomToken),
// owned by owner (a client ID, or "" for a caller with no lease -- see
// clientIDFromContext).
func (r *oauthFlowRegistry) register(flow workspace.OAuthFlow, owner string) string {
	id := randomToken()
	r.mu.Lock()
	r.byID[id] = &oauthFlowEntry{flow: flow, owner: owner}
	r.mu.Unlock()
	return id
}

// resolve looks up handle's flow without removing it: OAuthWait needs the
// flow to still be reachable while flow.Wait runs; the entry is dropped
// afterward, by release, not by resolve.
func (r *oauthFlowRegistry) resolve(handle string) (workspace.OAuthFlow, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byID[handle]
	if !ok {
		return nil, false
	}
	return entry.flow, true
}

// release drops handle from the registry and runs its flow's Cancel
// exactly once, regardless of which of the three call sites gets there
// first: OAuthWait once flow.Wait returns, the OAuthCancel RPC, or
// leaseManager.expire for a flow whose owner went quiet past the grace
// period. Reports whether handle was actually found, the same as
// handleRegistry.release, for a test to assert on; an unknown handle is
// not an error for any of the three callers.
func (r *oauthFlowRegistry) release(handle string) bool {
	r.mu.Lock()
	entry, ok := r.byID[handle]
	if ok {
		delete(r.byID, handle)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	entry.once.Do(entry.flow.Cancel)
	return true
}

// releaseByOwner cancels and drops every flow currently owned by owner --
// the lease sweep's action once a client has gone quiet past the grace
// period, so a vanished frontend never leaves a loopback callback listener
// bound (CLIENT-SERVER.md, PR 1.3b-2's build step 2; see
// internal/oauth/codex/oauth.go's fixed callback port).
func (r *oauthFlowRegistry) releaseByOwner(owner string) {
	r.mu.Lock()
	var ids []string
	for id, entry := range r.byID {
		if entry.owner == owner {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.release(id)
	}
}

// closeAll cancels and drops every flow still registered, regardless of
// owner -- NewServer's returned stop func calls this alongside the handle
// registry's own closeAll, so stopping the server never leaves a pending
// sign-in's resources held open.
func (r *oauthFlowRegistry) closeAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.release(id)
	}
}

// StartOAuth is class H: it calls the OAuth service's StartOAuth RPC and,
// when the server minted a flow handle, wraps it as a *clientOAuthFlow
// whose Wait/Cancel call OAuthWait/OAuthCancel -- see OAuthServer.OAuthWait
// for why Wait's ctx is not detached from the RPC the way AgentRunStream's
// turn is. FlowHandle is empty exactly when Result.Completed is set (see
// StartOAuthResponse), matching workspace.OAuthStartResult's contract that
// the accompanying OAuthFlow is nil then.
func (c *Client) StartOAuth(ctx context.Context, providerID, proxyURL string, forceNewAccount bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	req := &StartOAuthRequest{ProviderID: providerID, ProxyURL: proxyURL, ForceNewAccount: forceNewAccount}
	resp := new(StartOAuthResponse)
	fullMethod := "/" + oauthServiceName + "/StartOAuth"
	if err := c.invokeMethod(ctx, fullMethod, "StartOAuth", req, resp); err != nil {
		return workspace.OAuthStartResult{}, nil, err
	}
	if resp.FlowHandle == "" {
		return resp.Result, nil, nil
	}
	return resp.Result, &clientOAuthFlow{c: c, handle: resp.FlowHandle}, nil
}

// clientOAuthFlow is workspace.OAuthFlow's wire implementation.
type clientOAuthFlow struct {
	c      *Client
	handle string
}

// Wait opens the OAuth service's OAuthWait stream and returns its one
// frame, or the RPC's own error (identity-preserving, per
// decodeClientError) -- see OAuthServer.OAuthWait's doc comment for the
// ctx-cancels-the-wait contract this relies on.
func (f *clientOAuthFlow) Wait(ctx context.Context) (workspace.OAuthCompletion, error) {
	ctx = f.c.outgoingContext(ctx)
	desc := &oauthServiceDesc.Streams[0]
	fullMethod := "/" + oauthServiceName + "/OAuthWait"
	stream, err := f.c.conn.NewStream(ctx, desc, fullMethod, grpc.CallContentSubtype(jsonCodecName))
	if err != nil {
		return workspace.OAuthCompletion{}, decodeClientError("OAuthWait", err, nil)
	}
	req := &OAuthWaitRequest{FlowHandle: f.handle}
	if sendErr := stream.SendMsg(req); sendErr != nil {
		return workspace.OAuthCompletion{}, decodeClientError("OAuthWait", sendErr, stream.Trailer())
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return workspace.OAuthCompletion{}, decodeClientError("OAuthWait", closeErr, stream.Trailer())
	}

	frame := new(OAuthWaitFrame)
	if recvErr := stream.RecvMsg(frame); recvErr != nil {
		return workspace.OAuthCompletion{}, decodeClientError("OAuthWait", recvErr, stream.Trailer())
	}
	// One more Recv to observe the RPC's own completion (io.EOF on
	// success), draining the stream properly instead of abandoning it
	// mid-flight -- see Client.AgentRunShellCommand's identical tail Recv.
	tail := new(OAuthWaitFrame)
	if tailErr := stream.RecvMsg(tail); tailErr != nil && !errors.Is(tailErr, io.EOF) {
		return workspace.OAuthCompletion{}, decodeClientError("OAuthWait", tailErr, stream.Trailer())
	}
	return frame.Completion, nil
}

// Cancel calls the OAuth service's OAuthCancel RPC, bounded by this
// Client's own call timeout and logging (never returning) a failure --
// same as callHandles's release closure: a caller with a failed Cancel has
// nothing useful to do with the error beyond knowing it happened.
func (f *clientOAuthFlow) Cancel() {
	ctx, cancel := context.WithTimeout(context.Background(), f.c.callTimeout)
	defer cancel()
	req := &OAuthCancelRequest{FlowHandle: f.handle}
	resp := new(OAuthCancelResponse)
	fullMethod := "/" + oauthServiceName + "/OAuthCancel"
	if err := f.c.invokeMethod(ctx, fullMethod, "OAuthCancel", req, resp); err != nil {
		slog.Error("Wsrpc failed to cancel OAuth flow", "handle", f.handle, "error", err)
	}
}
