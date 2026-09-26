package grpcws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/wireerr"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
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

// Client is workspace.Workspace's gRPC transport: every U/C method is
// generated (zz_generated_service.go); the S/H/X methods below are
// hand-written, matching loopback_manual.go's coverage of the same
// classes for *Loopback. It talks the "json" content-subtype (codec.go) to
// a server built by NewServer/RegisterWorkspaceServer.
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

// NewClient wraps conn (typically a *grpc.ClientConn, or a bufconn-dialed
// one in tests) as a workspace.Workspace. Absent WithClientID, a fresh
// random client ID is generated -- every Client that should be treated as
// a *different* lease (a different logical client, not a reconnect of an
// existing one) must go through NewClient rather than sharing an ID.
func NewClient(conn grpc.ClientConnInterface, opts ...ClientOption) *Client {
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	c := &Client{conn: conn, callTimeout: defaultCallTimeout, clientID: randomToken(), lifeCtx: lifeCtx, lifeCancel: lifeCancel}
	for _, opt := range opts {
		opt(c)
	}
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

// Subscribe is class S: it opens the Events service's Subscribe stream
// and calls send for every event it decodes, blocking until ctx (this
// Client's own lifetime -- see Shutdown) is done. A broken stream is
// reconnected with capped backoff, replaying from the last Seq this
// client saw (or resyncing, if the server's buffer no longer has it) --
// see runSubscription. send also receives workspace.ConnectionEvent
// values reporting the stream's own health; the UI doesn't have a case
// for those yet (PR 1.4), which is harmless -- see root.go's default
// message routing.
func (c *Client) Subscribe(send func(any)) {
	c.runSubscription(c.lifeCtx, 0, send)
}

// SubscribeWith is class S; see Subscribe. It runs the same loop on its
// own goroutine, scoped to a child of this Client's lifetime so Shutdown
// still ends it, and returns a stop func that cancels just this
// subscription and waits for its goroutine to exit.
func (c *Client) SubscribeWith(send func(any)) func() {
	ctx, cancel := context.WithCancel(c.lifeCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runSubscription(ctx, 0, send)
	}()
	return func() {
		cancel()
		<-done
	}
}

// runSubscription is Subscribe/SubscribeWith's shared body: open the
// Events service's Subscribe stream at fromSeq, decode every EventFrame
// through wsrpc's event registry and hand the result to send, and on any
// stream error reconnect with backoff -- FromSeq = lastSeen+1, so a
// reconnect after a short blip replays exactly what was missed instead of
// either losing events or redelivering ones already seen. A Resync frame
// (the server's buffer didn't have what was asked for) is reported to
// send as a workspace.ConnectionEvent and, per its own doc comment,
// doesn't update lastSeen itself -- the next data frame's Seq does that,
// picking live delivery back up from wherever the server's hub currently
// is.
func (c *Client) runSubscription(ctx context.Context, fromSeq uint64, send func(any)) {
	backoff := initialReconnectBackoff
	everConnected := false
	for ctx.Err() == nil {
		stream, err := c.openEventStream(ctx, fromSeq)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if everConnected {
				send(connectionEvent(workspace.ConnectionLost))
				everConnected = false
			}
			if !sleepBackoff(ctx, &backoff) {
				return
			}
			continue
		}

		for {
			frame, err := stream.Recv()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if everConnected {
					send(connectionEvent(workspace.ConnectionLost))
				}
				everConnected = false
				break
			}
			backoff = initialReconnectBackoff
			if !everConnected {
				send(connectionEvent(workspace.ConnectionRecovered))
				everConnected = true
			}
			if frame.Resync {
				send(connectionEvent(workspace.ConnectionResync))
				continue
			}
			if frame.Event == nil {
				continue
			}
			v, err := wsrpc.DecodeEvent(*frame.Event)
			if err != nil {
				slog.Error("Wsrpc client failed to decode event frame, dropping it", "error", err)
				continue
			}
			send(v)
			fromSeq = frame.Seq + 1
		}

		if !sleepBackoff(ctx, &backoff) {
			return
		}
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
// (CLIENT-SERVER.md, PR 1.3, build step 4): it invokes the Handles
// service's RPC named name with req and, on success, builds the
// (Workspace, func(), error) every H method returns -- a new *Client
// sharing this one's connection and client-lease identity (so the server's
// lease tracks both as the same client -- see leaseManager) but bound to
// the handle the server just minted, and a release func that calls
// ReleaseHandle for it, bounded by this Client's own call timeout and
// logging (never returning) a failure: a caller done with a handle has
// nothing useful to do with a release error beyond knowing it happened.
func (c *Client) callHandles(ctx context.Context, name string, req any) (workspace.Workspace, func(), error) {
	resp := new(HandleResponse)
	fullMethod := "/" + handlesServiceName + "/" + name
	if err := c.invokeMethod(ctx, fullMethod, name, req, resp); err != nil {
		return nil, nil, err
	}
	handle := resp.Handle
	child := NewClient(c.conn, WithHandle(handle), WithClientID(c.clientID), WithCallTimeout(c.callTimeout))
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), c.callTimeout)
		defer cancel()
		if err := c.releaseHandle(releaseCtx, handle); err != nil {
			slog.Error("Wsrpc failed to release handle", "handle", handle, "error", err)
		}
	}
	return child, release, nil
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
// no Close) still has that part to add. What Shutdown does today (PR 1.2):
// cancel every running Subscribe/SubscribeWith, so neither blocks forever
// on a connection nobody is going to use again.
func (c *Client) Shutdown() {
	c.lifeCancel()
}
