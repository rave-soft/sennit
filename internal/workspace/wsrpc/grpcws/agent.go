package grpcws

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
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
//   - the turn itself runs on turnCtx, detached from the stream's own
//     cancellation (context.WithoutCancel, which keeps stream.Context()'s
//     values -- e.g. the resolved handle -- but not its Done channel): the
//     stream only *observes* the turn, per CLIENT-SERVER.md's PR 1.2 fix
//     ("Решение после ревью 2026-09-26"). A dropped connection therefore
//     no longer stops the turn; the only way to stop it is an explicit
//     AgentCancel(sessionID) call, which Client.AgentRunStream sends when
//     the caller's own ctx is cancelled (see its doc comment), and which
//     works independent of turnCtx because the coordinator tracks
//     cancellation by session ID, not by this RPC's context (see
//     agent.coordinator.Cancel / dispatcher's activeRequests registry);
//   - stream.Context() (streamCtx below) still governs *forwarding*: once
//     it's done (client gone, or the RPC itself cancelled), this handler
//     stops sending and returns, but keeps draining events in the
//     background so the turn -- now running unobserved -- can finish
//     without wedging its own goroutine on a full unbuffered channel. A
//     clean end of the turn while still observed (the events channel
//     closing after its terminal event) ends the RPC successfully (nil);
//     the last frame sent is always the terminal AgentRunEvent (Done:
//     true).
func (s *agentServer) AgentRunStream(req *AgentRunStreamRequest, stream AgentRunStreamServer) (err error) {
	// grpc-go does not recover a streaming handler's own panics -- see
	// eventsServer.Subscribe's identical guard.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Wsrpc AgentRunStream stream panicked, ending only this stream", "panic", r)
			err = status.Errorf(codes.Internal, "wsrpc: internal error")
		}
	}()

	streamCtx := stream.Context()
	ws, resolveErr := s.resolve(streamCtx)
	if resolveErr != nil {
		return grpcStatusFromError(streamCtx, resolveErr)
	}

	turnCtx := context.WithoutCancel(streamCtx)
	events, startErr := ws.AgentRunStream(turnCtx, req.SessionID, req.Prompt, req.Opts)
	if startErr != nil {
		return grpcStatusFromError(streamCtx, startErr)
	}

	if err := stream.Send(&AgentRunStreamFrame{Started: true}); err != nil {
		// The stream is already gone (client vanished between the two
		// sends); the turn keeps running on turnCtx regardless, so drain
		// its channel instead of leaking the goroutine that's blocked
		// producing into it.
		go drainAgentRunEvents(events)
		return err
	}
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(&AgentRunStreamFrame{Event: &ev}); err != nil {
				go drainAgentRunEvents(events)
				return err
			}
		case <-streamCtx.Done():
			// Only observation ends here -- the turn itself runs on
			// turnCtx and is unaffected. Keep draining so it can
			// complete normally.
			go drainAgentRunEvents(events)
			return streamCtx.Err()
		}
	}
}

// drainAgentRunEvents empties events until the turn producing them closes
// it, so a turn that outlives its stream (see AgentRunStream's doc
// comment) can still finish -- its sending goroutine would otherwise block
// forever on an abandoned unbuffered channel.
func drainAgentRunEvents(events <-chan workspace.AgentRunEvent) {
	for range events {
	}
}

// AgentRunShellCommand implements the semantics documented on
// AgentRunShellCommandFrame: stream.Context() is passed straight through
// to ws.AgentRunShellCommand, so cancelling the call (or a dropped
// connection) cancels the command the same way an in-process caller
// cancelling ctx does.
//
// Unlike AgentRunStream, this one does NOT detach the turn from the
// stream's cancellation (CLIENT-SERVER.md, PR 1.2 build step 1.2c). That
// fix works only because AgentCancel(sessionID) gives the client an
// independent way to stop the thing running server-side (the coordinator
// tracks cancellation by session ID; see agent.coordinator.Cancel). A bang
// command has no such handle: internal/ui/model/shell.go's only cancel
// path (m.editor.bang.cancelRunning, wired in runShellCommandInternal) is
// context.CancelFunc on the very ctx passed to
// workspace.AgentRunShellCommand, and AppWorkspace.AgentRunShellCommand
// (appws/app_workspace_agent.go) runs the command directly against that
// ctx -- there is no session- or command-ID-keyed way to reach it
// otherwise. Detaching here would make a running shell command
// uncancellable over the wire (surviving a disconnect at the cost of
// never being stoppable by the one existing mechanism), so it keeps
// today's behavior: stream.Context() is passed straight through, and a
// dropped connection still ends the command early, same as before this
// PR.
//
// onProgress calls stream.Send on whatever goroutine
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
// The turn no longer stops just because this stream ends (see
// agentServer.AgentRunStream's doc comment): cancelling ctx therefore also
// sends an explicit AgentCancel(sessionID) -- a bounded, best-effort call
// on its own background context (see the generated Client.AgentCancel) --
// before the terminal event derived from ctx.Err() is delivered. A
// transport failure that is NOT the caller's own cancellation (ctx.Err()
// == nil: the server or connection is what's actually gone) does not send
// AgentCancel; the caller gets the terminal ErrServerUnreachable event as
// before, and there is nothing to cancel on a server that can't be
// reached anyway. The terminal event is sent best-effort, bounded by
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
	//
	// ctx.Err() != nil here means the caller cancelled before the Started
	// ack arrived, not that the server ever reported a real synchronous
	// start error (decodeClientError's own doc comment: a caller
	// cancellation decodes to context.Canceled/DeadlineExceeded "whether
	// or not the server ever ran the call"). SendMsg/CloseSend above
	// already succeeded, so the request reached the server, and
	// agentServer.AgentRunStream's own ws.AgentRunStream call -- on
	// turnCtx, detached from this cancellation -- may already be running.
	// Send AgentCancel the same as the streaming loop below does once
	// it's underway: without this, a caller unlucky enough to cancel in
	// this narrow window before Started never sends it at all.
	first := new(AgentRunStreamFrame)
	if recvErr := stream.RecvMsg(first); recvErr != nil {
		if ctx.Err() != nil {
			_ = c.AgentCancel(sessionID)
		}
		return nil, decodeClientError("AgentRunStream", recvErr, stream.Trailer())
	}

	out := make(chan workspace.AgentRunEvent)
	go func() {
		defer close(out)

		// notifyCancel sends AgentCancel(sessionID) at most once, and only
		// from a call site that has already confirmed ctx.Err() != nil --
		// i.e. the caller's own ctx was cancelled, not a transport
		// failure. It runs synchronously (on this goroutine) so the
		// terminal event below is genuinely delivered "after", per this
		// method's doc comment; it can't wedge this goroutine because
		// Client.AgentCancel carries its own bounded timeout regardless of
		// ctx.
		var cancelOnce sync.Once
		notifyCancel := func() {
			cancelOnce.Do(func() {
				_ = c.AgentCancel(sessionID)
			})
		}

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
				if ctx.Err() != nil {
					notifyCancel()
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
				notifyCancel()
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
