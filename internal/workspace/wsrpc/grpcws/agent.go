package grpcws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
)

// agentServiceName is the third hand-written service (alongside Meta and
// Events): AgentRunStream and AgentRunShellCommand are class S
// (CLIENT-SERVER.md, PR 1.2 build step 1) -- server-stream RPCs the
// generator emits nothing for, same reasoning as eventsServiceName.
const agentServiceName = "sennit.workspace.v1.Agent"

// AgentRunStreamRequest is AgentRunStream's request.
type AgentRunStreamRequest struct {
	SessionID string                    `json:"session_id"`
	Prompt    string                    `json:"prompt"`
	Opts      workspace.AgentRunOptions `json:"opts"`
}

// AgentRunStreamFrame is one message of AgentRunStream's response stream.
// Started is sent exactly once, as the first frame, the instant the
// server's ws.AgentRunStream call itself returns successfully -- before
// anything the turn produces is available. Its only job is to let
// Client.AgentRunStream return (out, nil) as soon as the turn is
// genuinely accepted, without blocking on the first real AgentRunEvent
// (which can be seconds away, e.g. waiting on the first token): peeking
// at a plain AgentRunEvent frame instead would make the client call block
// until that first token, changing "returns once accepted" into "returns
// once something happened" (see workspace.go's doc comment on
// AgentRunStream). A synchronous start error never reaches this frame at
// all -- it fails the RPC itself, via the trailer (see
// grpcStatusFromError), before any frame is sent. Every later frame is a
// forwarded Event.
type AgentRunStreamFrame struct {
	Started bool                     `json:"started,omitempty"`
	Event   *workspace.AgentRunEvent `json:"event,omitempty"`
}

// AgentRunShellCommandRequest is AgentRunShellCommand's request.
type AgentRunShellCommandRequest struct {
	SessionID      string `json:"session_id"`
	Command        string `json:"command"`
	TermWidth      int    `json:"term_width"`
	IsFirstMessage bool   `json:"is_first_message"`
}

// AgentRunShellCommandFrame is one message of AgentRunShellCommand's
// response stream: a Progress chunk, or -- exactly once, last -- the
// final Response. Unlike AgentRunStream, AgentRunShellCommand has no
// separate "accepted" moment to preserve: in-process it is already one
// blocking call (see workspace.AgentController.AgentRunShellCommand), so
// there is nothing to protect by acking early. A failure is carried as
// the RPC's own error (trailer-encoded, identity-preserving -- see
// grpcStatusFromError) rather than an Err field here, matching every
// other Workspace method's error convention instead of inventing a
// second one just for this stream.
type AgentRunShellCommandFrame struct {
	Progress string                     `json:"progress,omitempty"`
	Done     bool                       `json:"done,omitempty"`
	Response proto.ShellCommandResponse `json:"response,omitempty"`
}

// AgentServer is the interface grpc.Server.RegisterService checks the
// registered handler against (see MetaServer's doc comment for why this
// can't just be *agentServer).
type AgentServer interface {
	AgentRunStream(*AgentRunStreamRequest, AgentRunStreamServer) error
	AgentRunShellCommand(*AgentRunShellCommandRequest, AgentRunShellCommandServer) error
}

// AgentRunStreamServer is the server side of the AgentRunStream stream.
type AgentRunStreamServer interface {
	Send(*AgentRunStreamFrame) error
	grpc.ServerStream
}

type agentRunStreamServer struct {
	grpc.ServerStream
}

func (x *agentRunStreamServer) Send(m *AgentRunStreamFrame) error {
	return x.SendMsg(m)
}

// AgentRunShellCommandServer is the server side of the
// AgentRunShellCommand stream.
type AgentRunShellCommandServer interface {
	Send(*AgentRunShellCommandFrame) error
	grpc.ServerStream
}

type agentRunShellCommandServer struct {
	grpc.ServerStream
}

func (x *agentRunShellCommandServer) Send(m *AgentRunShellCommandFrame) error {
	return x.SendMsg(m)
}

var agentServiceDesc = grpc.ServiceDesc{
	ServiceName: agentServiceName,
	HandlerType: (*AgentServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "AgentRunStream",
			Handler:       _Agent_AgentRunStream_Handler,
			ServerStreams: true,
		},
		{
			StreamName:    "AgentRunShellCommand",
			Handler:       _Agent_AgentRunShellCommand_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "wsrpc/agent",
}

func _Agent_AgentRunStream_Handler(srv any, stream grpc.ServerStream) error {
	m := new(AgentRunStreamRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(AgentServer).AgentRunStream(m, &agentRunStreamServer{stream})
}

func _Agent_AgentRunShellCommand_Handler(srv any, stream grpc.ServerStream) error {
	m := new(AgentRunShellCommandRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(AgentServer).AgentRunShellCommand(m, &agentRunShellCommandServer{stream})
}

// agentServer adapts a resolved workspace.Workspace to AgentServer.
// resolve mirrors workspaceServer's own (see zz_generated_service.go):
// PR 1.2 only ever serves the root handle; PR 1.3 adds non-root ones.
type agentServer struct {
	resolve func(ctx context.Context) (workspace.Workspace, error)
}

// AgentRunStream implements the semantics documented on
// AgentRunStreamFrame and workspace.go's own doc comment for
// Workspace.AgentRunStream:
//   - a synchronous start error (ws.AgentRunStream's own error return)
//     fails the RPC itself via grpcStatusFromError, before any frame is
//     sent, so the client decodes it with full identity
//     (errors.Is(err, workspace.ErrAgentNotInitialized) etc.) exactly as
//     it would in-process;
//   - otherwise a Started frame is sent immediately, then every
//     AgentRunEvent the turn produces, forwarded as it arrives;
//   - stream.Context() is passed straight through as the ctx
//     ws.AgentRunStream runs on, so this RPC's cancellation semantics
//     match AppWorkspace.AgentRunStream's exactly: the client cancelling
//     the call, and a dropped connection (which cancels the server
//     stream's context the same way), both stop the turn, because that
//     ctx is what AppWorkspace derives its own internal cancellation
//     from. A clean end of the turn (the events channel closing after its
//     terminal event) ends the RPC successfully (nil); the last frame
//     sent is always the terminal AgentRunEvent (Done: true).
func (s *agentServer) AgentRunStream(req *AgentRunStreamRequest, stream AgentRunStreamServer) (err error) {
	// grpc-go does not recover a streaming handler's own panics -- see
	// eventsServer.Subscribe's identical guard.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Wsrpc AgentRunStream stream panicked, ending only this stream", "panic", r)
			err = status.Errorf(codes.Internal, "wsrpc: internal error")
		}
	}()

	ctx := stream.Context()
	ws, resolveErr := s.resolve(ctx)
	if resolveErr != nil {
		return grpcStatusFromError(ctx, resolveErr)
	}

	events, startErr := ws.AgentRunStream(ctx, req.SessionID, req.Prompt, req.Opts)
	if startErr != nil {
		return grpcStatusFromError(ctx, startErr)
	}

	if err := stream.Send(&AgentRunStreamFrame{Started: true}); err != nil {
		return err
	}
	// Selecting on ctx.Done() as well as events, rather than a plain
	// `for ev := range events`, is a defensive measure matching
	// wsrpc.Loopback.AgentRunStream's own identical safety net: a
	// conforming Workspace always closes events once ctx is cancelled
	// (see workspace.go's doc comment on AgentRunStream), but this
	// handler would otherwise leak its goroutine forever against one
	// that doesn't, instead of ending the RPC as soon as the caller (or
	// a dropped connection) cancels.
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(&AgentRunStreamFrame{Event: &ev}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// AgentRunShellCommand implements the semantics documented on
// AgentRunShellCommandFrame: stream.Context() is passed straight through
// to ws.AgentRunShellCommand, so cancelling the call (or a dropped
// connection) cancels the command the same way an in-process caller
// cancelling ctx does. onProgress calls stream.Send on whatever goroutine
// AppWorkspace.AgentRunShellCommand calls it from -- the same "any
// goroutine, best effort" contract the in-process implementation already
// has (see internal/ui/model/shell.go's non-blocking send into its own
// channel): a Send failure here is swallowed rather than aborting the
// command early, because there is no cancellation hook from onProgress's
// signature (func(string), no error return) to report it through: the
// command keeps running to completion server-side, and the failure
// surfaces once AgentRunShellCommand itself returns, at the next Send or
// (if that also fails) as this RPC's own error.
func (s *agentServer) AgentRunShellCommand(req *AgentRunShellCommandRequest, stream AgentRunShellCommandServer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Wsrpc AgentRunShellCommand stream panicked, ending only this stream", "panic", r)
			err = status.Errorf(codes.Internal, "wsrpc: internal error")
		}
	}()

	ctx := stream.Context()
	ws, resolveErr := s.resolve(ctx)
	if resolveErr != nil {
		return grpcStatusFromError(ctx, resolveErr)
	}

	onProgress := func(chunk string) {
		_ = stream.Send(&AgentRunShellCommandFrame{Progress: chunk})
	}

	resp, cmdErr := ws.AgentRunShellCommand(ctx, req.SessionID, req.Command, req.TermWidth, onProgress, req.IsFirstMessage)
	if cmdErr != nil {
		return grpcStatusFromError(ctx, cmdErr)
	}
	return stream.Send(&AgentRunShellCommandFrame{Done: true, Response: resp})
}

// agentStreamUnreachableErr turns a broken AgentRunStream Recv (a
// transport failure with no trailer to decode -- the RPC never returned
// through the normal path) into the terminal AgentRunEvent a caller of
// Client.AgentRunStream is owed either way (see workspace.go's doc
// comment). ctx is the caller's own ctx, the one passed to
// Client.AgentRunStream: if it is already done, the break is the caller's
// own cancellation surfacing locally rather than a genuine server/network
// failure, so ctx.Err() is reported instead of ErrServerUnreachable --
// matching what an in-process cancellation would report.
func agentStreamUnreachableErr(ctx context.Context, method string, err error) *AgentRunStreamFrame {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &AgentRunStreamFrame{Event: &workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(ctxErr)}}
	}
	wrapped := fmt.Errorf("wsrpc: %s: %w: %w", method, workspace.ErrServerUnreachable, err)
	return &AgentRunStreamFrame{Event: &workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(wrapped)}}
}

// agentStreamTerminalSendTimeout bounds AgentRunStream's delivery of the one
// terminal event a broken/cancelled stream still owes a caller that has
// stopped reading -- see appws.terminalSendTimeout, whose reasoning this
// mirrors exactly (both exist so a consumer that has genuinely gone away
// can't wedge this goroutine forever).
const agentStreamTerminalSendTimeout = 200 * time.Millisecond

// AgentRunStream is class S: it opens the Agent service's AgentRunStream
// stream, translating the server's synchronous-start-error/Started-frame
// contract (see AgentRunStreamFrame) back into
// workspace.Workspace.AgentRunStream's own contract -- a synchronous
// error, or (channel, nil) returned as soon as the turn is accepted, not
// once anything has actually happened yet.
//
// Cancelling ctx (or the connection breaking) always yields a terminal
// event derived from ctx.Err(), exactly like the in-process
// implementation: ctx is the stream's own context, so cancelling it
// cancels the server-side call the same way an in-process ctx
// cancellation would (see agentServer.AgentRunStream's doc comment); the
// terminal event is sent best-effort, bounded by
// agentStreamTerminalSendTimeout, so a caller that has stopped reading
// entirely still can't wedge this goroutine open.
func (c *Client) AgentRunStream(ctx context.Context, sessionID, prompt string, opts workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	ctx = c.outgoingContext(ctx)
	desc := &agentServiceDesc.Streams[0]
	fullMethod := "/" + agentServiceName + "/AgentRunStream"
	stream, err := c.conn.NewStream(ctx, desc, fullMethod, grpc.CallContentSubtype(jsonCodecName))
	if err != nil {
		return nil, decodeClientError("AgentRunStream", err, nil)
	}
	req := &AgentRunStreamRequest{SessionID: sessionID, Prompt: prompt, Opts: opts}
	if sendErr := stream.SendMsg(req); sendErr != nil {
		return nil, decodeClientError("AgentRunStream", sendErr, stream.Trailer())
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return nil, decodeClientError("AgentRunStream", closeErr, stream.Trailer())
	}

	// The first frame is always either the Started ack (success) or this
	// Recv itself fails with the synchronous start error (see
	// AgentRunStreamFrame's doc comment) -- so it alone decides whether
	// this call returns a channel or an error, without waiting for
	// anything the turn produces.
	first := new(AgentRunStreamFrame)
	if recvErr := stream.RecvMsg(first); recvErr != nil {
		return nil, decodeClientError("AgentRunStream", recvErr, stream.Trailer())
	}

	out := make(chan workspace.AgentRunEvent)
	go func() {
		defer close(out)

		send := func(ev workspace.AgentRunEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		sendFinal := func(ev workspace.AgentRunEvent) {
			timer := time.NewTimer(agentStreamTerminalSendTimeout)
			defer timer.Stop()
			select {
			case out <- ev:
			case <-timer.C:
			}
		}

		for {
			frame := new(AgentRunStreamFrame)
			if recvErr := stream.RecvMsg(frame); recvErr != nil {
				if errors.Is(recvErr, io.EOF) {
					// The server ended the RPC without a terminal
					// (Done: true) event -- a wiring bug, not a
					// transport failure, but this caller is still owed
					// a terminal event rather than a silently closed
					// channel.
					sendFinal(workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(errors.New("wsrpc: AgentRunStream ended without a terminal event"))})
					return
				}
				final := agentStreamUnreachableErr(ctx, "AgentRunStream", recvErr)
				sendFinal(*final.Event)
				return
			}
			if frame.Event == nil {
				continue // a stray Started frame; only the first is expected, but ignore any more rather than crash on one.
			}
			ev := *frame.Event
			if ev.Done {
				sendFinal(ev)
				return
			}
			if !send(ev) {
				sendFinal(workspace.AgentRunEvent{Done: true, Err: workspace.EncodeError(ctx.Err())})
				return
			}
		}
	}()
	return out, nil
}

// AgentRunShellCommand is class S: it opens the Agent service's
// AgentRunShellCommand stream, calling onProgress for each Progress frame
// on whatever goroutine reads the stream (this one -- the call itself
// blocks until the final frame, matching the in-process contract exactly:
// see AgentRunShellCommandFrame's doc comment), and returns the final
// Response. A failure (including a broken connection, which decodes to
// workspace.ErrServerUnreachable per decodeClientError) is returned
// exactly as any other Workspace method's error, preserving identity.
func (c *Client) AgentRunShellCommand(ctx context.Context, sessionID, command string, termWidth int, onProgress func(string), isFirstMessage bool) (proto.ShellCommandResponse, error) {
	ctx = c.outgoingContext(ctx)
	desc := &agentServiceDesc.Streams[1]
	fullMethod := "/" + agentServiceName + "/AgentRunShellCommand"
	stream, err := c.conn.NewStream(ctx, desc, fullMethod, grpc.CallContentSubtype(jsonCodecName))
	if err != nil {
		return proto.ShellCommandResponse{}, decodeClientError("AgentRunShellCommand", err, nil)
	}
	req := &AgentRunShellCommandRequest{
		SessionID: sessionID, Command: command, TermWidth: termWidth, IsFirstMessage: isFirstMessage,
	}
	if sendErr := stream.SendMsg(req); sendErr != nil {
		return proto.ShellCommandResponse{}, decodeClientError("AgentRunShellCommand", sendErr, stream.Trailer())
	}
	if closeErr := stream.CloseSend(); closeErr != nil {
		return proto.ShellCommandResponse{}, decodeClientError("AgentRunShellCommand", closeErr, stream.Trailer())
	}

	for {
		frame := new(AgentRunShellCommandFrame)
		if recvErr := stream.RecvMsg(frame); recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				return proto.ShellCommandResponse{}, fmt.Errorf(
					"wsrpc: AgentRunShellCommand ended without a final frame: %w", workspace.ErrServerUnreachable)
			}
			return proto.ShellCommandResponse{}, decodeClientError("AgentRunShellCommand", recvErr, stream.Trailer())
		}
		if frame.Done {
			// One more Recv to observe the RPC's own completion (io.EOF
			// on success): draining the stream properly instead of
			// abandoning it mid-flight, matching the pattern
			// runSubscription/eventStreamClient use elsewhere in this
			// package.
			tail := new(AgentRunShellCommandFrame)
			if tailErr := stream.RecvMsg(tail); tailErr != nil && !errors.Is(tailErr, io.EOF) {
				return proto.ShellCommandResponse{}, decodeClientError("AgentRunShellCommand", tailErr, stream.Trailer())
			}
			return frame.Response, nil
		}
		if onProgress != nil {
			onProgress(frame.Progress)
		}
	}
}
