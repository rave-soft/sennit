package grpcws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/wireerr"
	"github.com/rave-soft/sennit/internal/workspace"
)

// handleMetadataKey is the outgoing metadata key a Client attaches to every
// call, naming which workspace.Workspace the server should resolve --
// CLIENT-SERVER.md, PR 1.1 build step 2. PR 1.1 only ever has the root
// handle (""); PR 1.3 adds non-root ones from EnterWorktree/ExitWorktree/
// AttachThread.
const handleMetadataKey = "sennit-handle"

// defaultCallTimeout is applied to a call whose Workspace method has no
// context.Context parameter of its own -- see WithCallTimeout.
const defaultCallTimeout = 30 * time.Second

// Client is workspace.Workspace's gRPC transport: every U method is
// generated (zz_generated_service.go); the S/H/X methods below are
// hand-written, matching loopback_manual.go's coverage of the same
// classes for *Loopback. Every C method is also hand-written
// (client_getters.go): it reads Client's own cache, filled by Connect and
// kept current by the internal event pump (client_pump.go), rather than
// calling the server at all -- see workspace.ClientState and CLIENT-
// SERVER.md, PR 1.4b. It talks the "json" content-subtype (codec.go) to a
// server built by NewServer/RegisterWorkspaceServer.
type Client struct {
	conn        grpc.ClientConnInterface
	handle      string
	clientID    string
	callTimeout time.Duration

	// lifeCtx/lifeCancel bound every subscription's lifetime: Subscribe
	// rides lifeCtx directly ("blocks until Shutdown"), SubscribeWith
	// derives its own child of it (its stop func cancels only that
	// child). Shutdown cancels lifeCtx, ending every running
	// subscription without touching conn -- PR 1.1 already decided
	// Shutdown doesn't own the connection (see Shutdown's doc comment).
	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	// parentLifeCtx, if set (withParentLifeCtx), is the context lifeCtx is
	// derived from instead of context.Background() -- callHandles sets it
	// to the parent Client's own lifeCtx, so a child handle's pump
	// (started by its own Connect call) stops automatically when the
	// parent shuts down, rather than leaking a goroutine that keeps
	// reconnecting through the shared conn forever after a caller
	// abandons the handle without ever calling its release func
	// (CLIENT-SERVER.md, PR 1.4b, build step 4).
	parentLifeCtx context.Context

	// connectOnce guards Connect's actual work: a Subscribe/SubscribeWith
	// call that finds no pump running yet calls Connect itself (see
	// ensureConnected), and a caller may also call Connect explicitly --
	// either way, only the first call does anything.
	connectOnce sync.Once
	connectErr  error

	// mu guards every field below: the cached workspace.ClientState, the
	// pending permission/question requests the pump is tracking, and the
	// live subscriber set. It is held across a subscriber's send call
	// (dispatch/attachSubscriber, client_pump.go), which is what keeps a
	// newly attached subscriber's initial pending-prompt replay from
	// interleaving with a concurrent live dispatch for the same
	// subscriber (CLIENT-SERVER.md, PR 1.4b, build step 2) --
	// correctness over throughput, since a C getter never blocks on this
	// lock for longer than a slice/map read.
	mu                 sync.Mutex
	haveState          bool
	state              workspace.ClientState
	pendingPermissions map[string]permission.PermissionRequest
	pendingQuestions   map[string]question.Request
	subs               map[*clientSub]struct{}

	// pumpCancel/pumpDone belong to the single internal event pump Connect
	// starts (runPump): pumpCancel ends it, pumpDone closes once it has.
	// Both are nil until Connect's first successful run.
	pumpCancel context.CancelFunc
	pumpDone   chan struct{}

	// noConnectionOnce logs, once per Client, that a class-C getter was
	// asked for before Connect ever completed -- CLIENT-SERVER.md, PR
	// 1.4b build step 3 ("before the first successful Connect they return
	// zero values and log once per client").
	noConnectionOnce sync.Once
}

// clientSub is one Subscribe/SubscribeWith attachment to this Client's
// internal event pump (see attachSubscriber/detachSubscriber).
type clientSub struct {
	send func(any)
}

// ClientOption configures a Client at construction (NewClient).
type ClientOption func(*Client)

// WithCallTimeout overrides defaultCallTimeout: the deadline given to a
// context.WithTimeout call built for a Workspace method that takes no
// ctx of its own.
func WithCallTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.callTimeout = d }
}

// WithHandle targets calls at a non-root workspace handle (PR 1.3). Every
// call before that PR lands only ever sees the root handle, the zero
// value.
func WithHandle(handle string) ClientOption {
	return func(c *Client) { c.handle = handle }
}

// WithClientID pins this Client's lease identity (the "sennit-client"
// metadata every call carries -- see leaseManager) instead of the random
// one NewClient otherwise generates. A client reconnecting after a dropped
// connection passes the same ID its previous *Client used
// (Client.ClientID), so the server's lease sees the same client rather
// than a brand-new one with no handles yet -- CLIENT-SERVER.md, PR 1.3.
func WithClientID(id string) ClientOption {
	return func(c *Client) { c.clientID = id }
}

// withParentLifeCtx is unexported: only callHandles uses it (see
// parentLifeCtx's own doc comment on the Client struct).
func withParentLifeCtx(ctx context.Context) ClientOption {
	return func(c *Client) { c.parentLifeCtx = ctx }
}

// NewClient wraps conn (typically a *grpc.ClientConn, or a bufconn-dialed
// one in tests) as a workspace.Workspace. Absent WithClientID, a fresh
// random client ID is generated -- every Client that should be treated as
// a *different* lease (a different logical client, not a reconnect of an
// existing one) must go through NewClient rather than sharing an ID.
func NewClient(conn grpc.ClientConnInterface, opts ...ClientOption) *Client {
	c := &Client{conn: conn, callTimeout: defaultCallTimeout, clientID: randomToken()}
	for _, opt := range opts {
		opt(c)
	}
	parent := context.Background()
	if c.parentLifeCtx != nil {
		parent = c.parentLifeCtx
	}
	c.lifeCtx, c.lifeCancel = context.WithCancel(parent)
	return c
}

var _ workspace.Workspace = (*Client)(nil)

// ClientID is this Client's lease identity, as sent in the "sennit-client"
// metadata on every call (see leaseManager). A caller reconnecting after a
// dropped connection passes it to WithClientID on the replacement Client.
func (c *Client) ClientID() string { return c.clientID }

// Handle is the workspace handle this Client targets ("" for the root),
// as sent in the "sennit-handle" metadata on every call. A caller that
// needs to rebuild a *Client bound to the same non-root handle -- after a
// reconnect within the server's lease grace (CLIENT-SERVER.md, PR 1.3),
// say -- passes it to WithHandle on the replacement Client.
func (c *Client) Handle() string { return c.handle }

// invoke calls the Workspace service's method RPC, attaching c's handle
// metadata and selecting the "json" codec, and translates any failure per
// this package's doc comment on errors (see codec.go's errorTrailerKey and
// decodeClientError below).
func (c *Client) invoke(ctx context.Context, method string, req, resp any) error {
	return c.invokeMethod(ctx, "/"+serviceName+"/"+method, method, req, resp)
}

// outgoingContext attaches this Client's handle and client-lease metadata
// to ctx -- every call this package makes, unary or streaming, needs both
// (see handleMetadataKey/clientMetadataKey's own doc comments).
func (c *Client) outgoingContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, handleMetadataKey, c.handle, clientMetadataKey, c.clientID)
}

// decodeClientError turns a failed Invoke's error into the shape callers
// expect (CLIENT-SERVER.md, PR 1.1, build step 4):
//   - a context cancellation or deadline the caller itself triggered comes
//     back as errors.Is(err, context.Canceled) /
//     errors.Is(err, context.DeadlineExceeded), whether or not the server
//     ever ran the call;
//   - a call that reached the server and failed there decodes the
//     trailer's WireError (workspace.DecodeError), preserving the
//     original error's identity;
//   - anything else (connection refused, server gone, no trailer) wraps
//     workspace.ErrServerUnreachable.
func decodeClientError(method string, err error, trailer metadata.MD) error {
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.Canceled:
			return context.Canceled
		case codes.DeadlineExceeded:
			return context.DeadlineExceeded
		}
	}
	if vals := trailer.Get(errorTrailerKey); len(vals) > 0 {
		var we wireerr.Error
		if jsonErr := json.Unmarshal([]byte(vals[0]), &we); jsonErr == nil {
			return workspace.DecodeError(&we)
		}
	}
	return fmt.Errorf("wsrpc: calling %s: %w: %w", method, workspace.ErrServerUnreachable, err)
}

// grpcStatusFromError is the server-side half of the error contract above:
// it encodes err as a WireError, attaches it to ctx's outgoing trailer
// under errorTrailerKey, and returns a *status.Status whose code is chosen
// from the WireError's code -- close enough for gRPC's own machinery
// (health checks, deadlines) to behave sensibly, while the trailer carries
// the real, identity-preserving payload the client actually decodes.
func grpcStatusFromError(ctx context.Context, err error) error {
	we := workspace.EncodeError(err)
	if data, marshalErr := json.Marshal(we); marshalErr == nil {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(errorTrailerKey, string(data)))
	}
	code := codes.Unknown
	switch we.Code {
	case "canceled":
		code = codes.Canceled
	case "deadline_exceeded":
		code = codes.DeadlineExceeded
	case "session_not_found", "workspace_gone":
		code = codes.NotFound
	case "read_only":
		code = codes.PermissionDenied
	}
	return status.Error(code, we.Message)
}

// initialReconnectBackoff and maxReconnectBackoff bound
// runSubscription's reconnect delay (CLIENT-SERVER.md, PR 1.2/1.4): it
// starts at initialReconnectBackoff and doubles on each further failure,
// capped at maxReconnectBackoff, and resets to initialReconnectBackoff
// the moment a reconnect succeeds (a frame is actually received).
const (
	initialReconnectBackoff = 250 * time.Millisecond
	maxReconnectBackoff     = 10 * time.Second
)

// Subscribe is class S: it attaches send to this Client's internal event
// pump (starting it via Connect if this is the first Subscribe/
// SubscribeWith/Connect call -- see ensureConnected) and blocks until ctx
// (this Client's own lifetime -- see Shutdown) is done. send first
// receives every permission/question request the pump currently considers
// pending (see attachSubscriber), then every event the pump goes on to
// dispatch, including the synthesized workspace.ConnectionEvent values
// reporting the stream's own health.
func (c *Client) Subscribe(send func(any)) {
	c.ensureConnected(c.lifeCtx)
	sub := &clientSub{send: send}
	c.attachSubscriber(sub)
	defer c.detachSubscriber(sub)
	<-c.lifeCtx.Done()
}

// SubscribeWith is class S; see Subscribe. It runs the same wait on its
// own goroutine, scoped to a child of this Client's lifetime so Shutdown
// still ends it, and returns a stop func that cancels just this
// subscription and waits for its goroutine to exit.
func (c *Client) SubscribeWith(send func(any)) func() {
	c.ensureConnected(c.lifeCtx)
	ctx, cancel := context.WithCancel(c.lifeCtx)
	sub := &clientSub{send: send}
	c.attachSubscriber(sub)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		c.detachSubscriber(sub)
	}()
	return func() {
		cancel()
		<-done
	}
}

// connectionEvent wraps state as the pubsub.Event[workspace.ConnectionEvent]
// send expects -- the same shape every other registered event travels as
// (see wsrpc/events.go), even though this one is never actually encoded
// across the wire: it's synthesized here, client-side, from the stream's
// own health.
func connectionEvent(state workspace.ConnectionState) pubsub.Event[workspace.ConnectionEvent] {
	return pubsub.Event[workspace.ConnectionEvent]{Type: pubsub.UpdatedEvent, Payload: workspace.ConnectionEvent{State: state}}
}

// openEventStream opens one attempt at the Events service's Subscribe
// stream, attaching this Client's handle metadata like every other call
// (see invoke).
func (c *Client) openEventStream(ctx context.Context, fromSeq uint64) (grpc.ServerStreamingClient[EventFrame], error) {
	ctx = c.outgoingContext(ctx)
	desc := &eventsServiceDesc.Streams[0]
	fullMethod := "/" + eventsServiceName + "/Subscribe"
	stream, err := c.conn.NewStream(ctx, desc, fullMethod, grpc.CallContentSubtype(jsonCodecName))
	if err != nil {
		return nil, err
	}
	req := &SubscribeRequest{FromSeq: fromSeq}
	if err := stream.SendMsg(req); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return &eventStreamClient{stream}, nil
}

// eventStreamClient adapts the untyped grpc.ClientStream NewStream hands
// back into grpc.ServerStreamingClient[EventFrame]'s typed Recv, the way
// generated code normally would.
type eventStreamClient struct {
	grpc.ClientStream
}

func (x *eventStreamClient) Recv() (*EventFrame, error) {
	m := new(EventFrame)
	if err := x.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// sleepBackoff waits backoff (or until ctx is done, whichever first),
// then doubles *backoff up to maxReconnectBackoff. It reports whether the
// wait completed normally (false means ctx ended first, so the caller
// should stop).
func sleepBackoff(ctx context.Context, backoff *time.Duration) bool {
	timer := time.NewTimer(*backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return false
	}
	*backoff *= 2
	if *backoff > maxReconnectBackoff {
		*backoff = maxReconnectBackoff
	}
	return true
}

// EnterWorktree is class H: it calls the Handles service's EnterWorktree
// RPC, which acts on whatever workspace this Client currently targets (its
// own handle metadata), and wraps a successful response as the (Workspace,
// release) pair every H method returns -- see callHandles.
func (c *Client) EnterWorktree(ctx context.Context, name string) (workspace.Workspace, func(), error) {
	return c.callHandles(ctx, "EnterWorktree", &EnterWorktreeRequest{Name: name})
}

// ExitWorktree is class H; see EnterWorktree.
func (c *Client) ExitWorktree(ctx context.Context) (workspace.Workspace, func(), error) {
	return c.callHandles(ctx, "ExitWorktree", &ExitWorktreeRequest{})
}

// AttachThread is class H; see EnterWorktree.
func (c *Client) AttachThread(ctx context.Context, id string) (workspace.Workspace, func(), error) {
	return c.callHandles(ctx, "AttachThread", &AttachThreadRequest{ID: id})
}

// callHandles is EnterWorktree/ExitWorktree/AttachThread's shared body
// (CLIENT-SERVER.md, PR 1.3, build step 4; PR 1.4b, build step 4): it
// invokes the Handles service's RPC named name with req and, on success,
// builds the (Workspace, func(), error) every H method returns -- a new
// *Client sharing this one's connection and client-lease identity (so the
// server's lease tracks both as the same client -- see leaseManager) but
// bound to the handle the server just minted. The child is Connect-ed
// before it is handed back, so it has its own cache and pump seeded from
// its own handle's Snapshot (WorkingDir/WorktreeState and the rest of
// ClientState are per-handle, not inherited from the parent). release
// stops the child's pump (Shutdown) and calls ReleaseHandle for the
// parent's connection, bounded by this Client's own call timeout and
// logging (never returning) a failure: a caller done with a handle has
// nothing useful to do with a release error beyond knowing it happened.
func (c *Client) callHandles(ctx context.Context, name string, req any) (workspace.Workspace, func(), error) {
	resp := new(HandleResponse)
	fullMethod := "/" + handlesServiceName + "/" + name
	if err := c.invokeMethod(ctx, fullMethod, name, req, resp); err != nil {
		return nil, nil, err
	}
	handle := resp.Handle
	child := NewClient(c.conn, WithHandle(handle), WithClientID(c.clientID), WithCallTimeout(c.callTimeout), withParentLifeCtx(c.lifeCtx))
	if err := child.Connect(ctx); err != nil {
		child.Shutdown()
		c.releaseHandleLogged(handle)
		return nil, nil, fmt.Errorf("wsrpc: connecting handle %s: %w", handle, err)
	}
	release := func() {
		child.Shutdown()
		c.releaseHandleLogged(handle)
	}
	return child, release, nil
}

// releaseHandleLogged calls releaseHandle for handle, bounded by this
// Client's own call timeout, logging (never returning) a failure.
func (c *Client) releaseHandleLogged(handle string) {
	releaseCtx, cancel := context.WithTimeout(context.Background(), c.callTimeout)
	defer cancel()
	if err := c.releaseHandle(releaseCtx, handle); err != nil {
		slog.Error("Wsrpc failed to release handle", "handle", handle, "error", err)
	}
}

// releaseHandle calls the Handles service's ReleaseHandle RPC for handle.
func (c *Client) releaseHandle(ctx context.Context, handle string) error {
	req := &ReleaseHandleRequest{Handle: handle}
	resp := new(ReleaseHandleResponse)
	fullMethod := "/" + handlesServiceName + "/ReleaseHandle"
	return c.invokeMethod(ctx, fullMethod, "ReleaseHandle", req, resp)
}

// Shutdown is class X. On a real connection this would also close it and
// release any handles the client holds (CLIENT-SERVER.md's method table:
// "в клиенте закрывает соединение и освобождает хэндлы; демон не
// трогает"), but PR 1.1 doesn't own the connection's lifecycle -- whatever
// dialed conn also closes it -- so a later PR that gives Client its own
// *grpc.ClientConn (rather than a bare grpc.ClientConnInterface, which has
// no Close) still has that part to add. What Shutdown does today: cancel
// every running Subscribe/SubscribeWith (PR 1.2) and stop the internal
// event pump Connect started, if any (PR 1.4b, build step 5), waiting for
// its goroutine to actually exit so a caller that checks for goroutine
// leaks right after Shutdown doesn't see one that just hasn't been asked
// to exit yet.
func (c *Client) Shutdown() {
	c.lifeCancel()
	c.mu.Lock()
	done := c.pumpDone
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}
