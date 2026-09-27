package grpcws

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// eventsServiceName is the small second service (like metaServiceName)
// that carries Subscribe -- a hand-written server-stream RPC, so it
// doesn't belong in the generated, unary-only Workspace service (see
// classes.go's Class S).
const eventsServiceName = "sennit.workspace.v1.Events"

// SubscribeRequest is Subscribe's request: FromSeq == 0 means "live
// events only, starting now"; FromSeq > 0 asks the server to replay
// anything buffered from that Seq onward first (CLIENT-SERVER.md, PR 1.2
// build step 2).
type SubscribeRequest struct {
	FromSeq uint64 `json:"from_seq"`
}

// EventFrame is one message of Subscribe's response stream. Event is nil
// exactly on a Resync frame -- Seq is meaningless there too; the client
// picks its next FromSeq back up from the first data frame that follows
// (see grpcws.Client.Subscribe's doc comment).
type EventFrame struct {
	Seq    uint64          `json:"seq,omitempty"`
	Event  *wsrpc.Envelope `json:"event,omitempty"`
	Resync bool            `json:"resync,omitempty"`
}

// SnapshotRequest is Snapshot's (empty) request.
type SnapshotRequest struct{}

// SnapshotResponse is Snapshot's result: everything a client needs to seed
// its cache and its pending-prompt dialogs before subscribing from Seq+1
// (CLIENT-SERVER.md, PR 1.4a). PendingPermissions/PendingQuestions cover
// requests that were already outstanding when this client connected -- a
// request is announced on the event stream exactly once, when it is
// raised, so a client with no other way to have seen that announcement
// needs this to learn about one.
type SnapshotResponse struct {
	Seq                uint64                         `json:"seq"`
	State              workspace.ClientState          `json:"state"`
	PendingPermissions []permission.PermissionRequest `json:"pending_permissions,omitempty"`
	PendingQuestions   []question.Request             `json:"pending_questions,omitempty"`
}

// WorkspaceEventsServer is the interface grpc.Server.RegisterService
// checks the registered handler against (see MetaServer's doc comment for
// why this can't just be *eventsServer).
type WorkspaceEventsServer interface {
	Subscribe(*SubscribeRequest, WorkspaceEventsSubscribeServer) error
	Snapshot(context.Context, *SnapshotRequest) (*SnapshotResponse, error)
}

// WorkspaceEventsSubscribeServer is the server side of the Subscribe
// stream: Send plus the usual grpc.ServerStream context/header plumbing.
type WorkspaceEventsSubscribeServer interface {
	Send(*EventFrame) error
	grpc.ServerStream
}

type workspaceEventsSubscribeServer struct {
	grpc.ServerStream
}

func (x *workspaceEventsSubscribeServer) Send(m *EventFrame) error {
	return x.SendMsg(m)
}

var eventsServiceDesc = grpc.ServiceDesc{
	ServiceName: eventsServiceName,
	HandlerType: (*WorkspaceEventsServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Snapshot", Handler: _WorkspaceEvents_Snapshot_Handler},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "Subscribe",
			Handler:       _WorkspaceEvents_Subscribe_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "wsrpc/events",
}

func _WorkspaceEvents_Subscribe_Handler(srv any, stream grpc.ServerStream) error {
	m := new(SubscribeRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(WorkspaceEventsServer).Subscribe(m, &workspaceEventsSubscribeServer{stream})
}

func _WorkspaceEvents_Snapshot_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(SnapshotRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(WorkspaceEventsServer).Snapshot(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + eventsServiceName + "/Snapshot"}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(WorkspaceEventsServer).Snapshot(ctx, req.(*SnapshotRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// eventsServer adapts one or more eventHubs to WorkspaceEventsServer.
// resolveHub mirrors NewServer's handle resolution for the unary service:
// PR 1.2 only ever serves the root handle ("" from handleFromContext);
// PR 1.3 adds non-root ones. resolve is the same handle resolution down to
// the workspace.Workspace itself (not just its hub), which Snapshot needs
// for PendingPrompts -- the hub alone knows the last published Seq/State,
// but not what's currently awaiting an answer.
type eventsServer struct {
	resolveHub func(ctx context.Context) (*eventHub, error)
	resolve    func(ctx context.Context) (workspace.Workspace, error)
}

// Snapshot implements WorkspaceEventsServer: it reads the hub's last
// published Seq and workspace.ClientState as one atomic pair (see
// eventHub.snapshot's own doc comment for why that ordering is what makes
// a client's "subscribe from Seq+1" safe against an event landing in
// between), then collects whatever permission/question requests are
// currently awaiting an answer with no subscriber yet -- CLIENT-SERVER.md,
// PR 1.4a.
func (s *eventsServer) Snapshot(ctx context.Context, _ *SnapshotRequest) (*SnapshotResponse, error) {
	hub, err := s.resolveHub(ctx)
	if err != nil {
		return nil, err
	}
	seq, state := hub.snapshot()

	ws, err := s.resolve(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	pending, err := ws.PendingPrompts(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}

	return &SnapshotResponse{
		Seq:                seq,
		State:              state,
		PendingPermissions: pending.Permissions,
		PendingQuestions:   pending.Questions,
	}, nil
}

// Snapshot calls the Events service's Snapshot RPC: everything a client
// needs to seed its cache before subscribing from the returned Seq+1 (see
// SnapshotResponse's own doc comment).
func (c *Client) Snapshot(ctx context.Context) (SnapshotResponse, error) {
	req := &SnapshotRequest{}
	resp := new(SnapshotResponse)
	fullMethod := "/" + eventsServiceName + "/Snapshot"
	if err := c.invokeMethod(ctx, fullMethod, "Snapshot", req, resp); err != nil {
		return SnapshotResponse{}, err
	}
	return *resp, nil
}

// Subscribe implements the semantics documented on SubscribeRequest/
// EventFrame: it registers with the hub before deciding what to replay
// (see eventHub.subscribe), sends a leading Resync frame if the client's
// FromSeq can't be satisfied from the buffer, replays whatever can be,
// then streams live until the client disconnects or the subscriber is
// itself resynced for falling behind (hubSubscriber.deliver).
func (s *eventsServer) Subscribe(req *SubscribeRequest, stream WorkspaceEventsSubscribeServer) (err error) {
	// grpc-go does not recover a streaming handler's own panics -- an
	// unrecovered one here would take down the whole server, not just
	// this client's stream (CLIENT-SERVER.md, PR 1.2 build step 3).
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Wsrpc Subscribe stream panicked, ending only this stream", "panic", r)
			err = status.Errorf(codes.Internal, "wsrpc: internal error")
		}
	}()

	hub, resolveErr := s.resolveHub(stream.Context())
	if resolveErr != nil {
		return resolveErr
	}

	sub, replay, resync := hub.subscribe(req.FromSeq)
	defer hub.unsubscribe(sub)

	if resync {
		if err := stream.Send(&EventFrame{Resync: true}); err != nil {
			return err
		}
	}
	for _, e := range replay {
		env := e.env
		if err := stream.Send(&EventFrame{Seq: e.seq, Event: &env}); err != nil {
			return err
		}
	}
	if !resync && len(replay) == 0 {
		// Neither a Resync nor any replay frame went out above, so a
		// reconnect onto an idle stream (nothing published since) would
		// otherwise send nothing at all until the next real event -- the
		// client only dispatches ConnectionRecovered on a successful
		// Recv, so the "connection lost" indicator would stay stuck
		// forever on an idle daemon. An empty frame gives the client that
		// Recv immediately; the client already skips frames with
		// Event == nil without advancing FromSeq.
		if err := stream.Send(&EventFrame{}); err != nil {
			return err
		}
	}

	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-sub.ch:
			if !ok {
				// hubSubscriber.deliver closed us after an overflow (its
				// own best-effort Resync frame, if any, already went out
				// through this same channel and was sent above); ending
				// the stream cleanly lets the client's own reconnect
				// pick a fresh FromSeq.
				return nil
			}
			if err := stream.Send(&frame); err != nil {
				return err
			}
		}
	}
}
