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

	"github.com/rave-soft/sennit/internal/proto"
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

// Client is workspace.Workspace's gRPC transport: every U/C method is
// generated (zz_generated_service.go); the S/H/X methods below are
// hand-written, matching loopback_manual.go's coverage of the same
// classes for *Loopback. It talks the "json" content-subtype (codec.go) to
// a server built by NewServer/RegisterWorkspaceServer.
type Client struct {
	conn        grpc.ClientConnInterface
	handle      string
	callTimeout time.Duration
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

// NewClient wraps conn (typically a *grpc.ClientConn, or a bufconn-dialed
// one in tests) as a workspace.Workspace.
func NewClient(conn grpc.ClientConnInterface, opts ...ClientOption) *Client {
	c := &Client{conn: conn, callTimeout: defaultCallTimeout}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

var _ workspace.Workspace = (*Client)(nil)

// invoke calls the Workspace service's method RPC, attaching c's handle
// metadata and selecting the "json" codec, and translates any failure per
// this package's doc comment on errors (see codec.go's errorTrailerKey and
// decodeClientError below).
func (c *Client) invoke(ctx context.Context, method string, req, resp any) error {
	return c.invokeMethod(ctx, "/"+serviceName+"/"+method, method, req, resp)
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
	case "session_not_found":
		code = codes.NotFound
	case "read_only":
		code = codes.PermissionDenied
	}
	return status.Error(code, we.Message)
}

// notAvailableOverWire builds the error every S/H/X method below returns:
// PR 1.1 is the unary transport only -- streams (S) and handles (H) are
// hand-written in later PRs (1.2, 1.3).
func notAvailableOverWire(method string) error {
	return fmt.Errorf("wsrpc: %s is not available over this transport yet", method)
}

// AgentRunShellCommand is class S; not available until PR 1.2 wires up
// server streams.
func (c *Client) AgentRunShellCommand(context.Context, string, string, int, func(string), bool) (proto.ShellCommandResponse, error) {
	return proto.ShellCommandResponse{}, notAvailableOverWire("AgentRunShellCommand")
}

// AgentRunStream is class S; see AgentRunShellCommand.
func (c *Client) AgentRunStream(context.Context, string, string, workspace.AgentRunOptions) (<-chan workspace.AgentRunEvent, error) {
	return nil, notAvailableOverWire("AgentRunStream")
}

// Subscribe is class S; see AgentRunShellCommand. It has no error result,
// so it logs instead of silently doing nothing.
func (c *Client) Subscribe(func(any)) {
	logNotAvailable("Subscribe")
}

// SubscribeWith is class S; see Subscribe. The returned stop func is a
// no-op: there is nothing running to stop.
func (c *Client) SubscribeWith(func(any)) func() {
	logNotAvailable("SubscribeWith")
	return func() {}
}

// StartOAuth is class H; not available until PR 1.3 adds the handle
// registry.
func (c *Client) StartOAuth(context.Context, string, string, bool) (workspace.OAuthStartResult, workspace.OAuthFlow, error) {
	return workspace.OAuthStartResult{}, nil, notAvailableOverWire("StartOAuth")
}

// EnterWorktree is class H; see StartOAuth.
func (c *Client) EnterWorktree(context.Context, string) (workspace.Workspace, func(), error) {
	return nil, nil, notAvailableOverWire("EnterWorktree")
}

// ExitWorktree is class H; see StartOAuth.
func (c *Client) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	return nil, nil, notAvailableOverWire("ExitWorktree")
}

// AttachThread is class H; see StartOAuth.
func (c *Client) AttachThread(context.Context, string) (workspace.Workspace, func(), error) {
	return nil, nil, notAvailableOverWire("AttachThread")
}

// Shutdown is class X. On a real connection this would close it and
// release any handles the client holds (CLIENT-SERVER.md's method table:
// "в клиенте закрывает соединение и освобождает хэндлы; демон не
// трогает"), but PR 1.1 doesn't own the connection's lifecycle -- whatever
// dialed conn also closes it. Shutdown is therefore a deliberate no-op
// here; a later PR that gives Client its own *grpc.ClientConn (rather than
// a bare grpc.ClientConnInterface, which has no Close) can make it do
// that.
func (c *Client) Shutdown() {}

// logNotAvailable is notAvailableOverWire's counterpart for the two
// callback-style S methods (Subscribe, SubscribeWith) that have no error
// result to report a missing transport through.
func logNotAvailable(method string) {
	slog.Warn("Wsrpc method not available over this transport yet", "method", method)
}
