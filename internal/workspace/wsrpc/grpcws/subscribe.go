package grpcws

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

// WorkspaceEventsServer is the interface grpc.Server.RegisterService
// checks the registered handler against (see MetaServer's doc comment for
// why this can't just be *eventsServer).
type WorkspaceEventsServer interface {
	Subscribe(*SubscribeRequest, WorkspaceEventsSubscribeServer) error
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
	Methods:     []grpc.MethodDesc{},
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

// eventsServer adapts one or more eventHubs to WorkspaceEventsServer.
// resolveHub mirrors NewServer's handle resolution for the unary service:
// PR 1.2 only ever serves the root handle ("" from handleFromContext);
// PR 1.3 adds non-root ones.
type eventsServer struct {
	resolveHub func(ctx context.Context) (*eventHub, error)
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
