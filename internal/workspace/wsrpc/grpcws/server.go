package grpcws

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace"
)

// metaServiceName is the small second service (CLIENT-SERVER.md, PR 1.1,
// build step 5) that carries Hello -- a hand-written RPC with no
// workspace.Workspace method behind it, so it doesn't belong in the
// generated Workspace service.
const metaServiceName = "sennit.workspace.v1.Meta"

// ProtocolVersion is the wire contract version Hello reports. A client
// bumps its own expectation of this only when the Request/Response DTOs
// or the error/coding scheme change in a way an older build can't decode;
// a mismatch is meant to be shown to the user as "daemon is version X, run
// `sennit daemon restart`" (CLIENT-SERVER.md, PR 1.1), not silently
// tolerated.
const ProtocolVersion = 1

// HelloRequest is Hello's (empty) request.
type HelloRequest struct{}

// HelloResponse is Hello's result: enough for a client to tell whether it
// can talk to this server at all (ProtocolVersion) and, if not, say so
// usefully (Version/BuildID).
type HelloResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	Version         string `json:"version"`
	BuildID         string `json:"build_id"`
	WorkingDir      string `json:"working_dir"`
	ServerHome      string `json:"server_home"`
}

// MetaServer is the interface grpc.Server.RegisterService checks metaServer
// against (google.golang.org/grpc requires ServiceDesc.HandlerType to name
// an interface, not the concrete impl -- see grpc.Server.RegisterService).
type MetaServer interface {
	Hello(context.Context, *HelloRequest) (*HelloResponse, error)
}

type metaServer struct {
	workingDir func() string
	serverHome func() string
}

func (s *metaServer) Hello(context.Context, *HelloRequest) (*HelloResponse, error) {
	return &HelloResponse{
		ProtocolVersion: ProtocolVersion,
		Version:         version.Version,
		BuildID:         version.Commit,
		WorkingDir:      s.workingDir(),
		ServerHome:      s.serverHome(),
	}, nil
}

var metaServiceDesc = grpc.ServiceDesc{
	ServiceName: metaServiceName,
	HandlerType: (*MetaServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "Hello", Handler: _Meta_Hello_Handler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "wsrpc/meta",
}

func _Meta_Hello_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(HelloRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(MetaServer).Hello(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + metaServiceName + "/Hello"}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(MetaServer).Hello(ctx, req.(*HelloRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// Hello calls the Meta service's Hello RPC.
func (c *Client) Hello(ctx context.Context) (HelloResponse, error) {
	req := &HelloRequest{}
	resp := new(HelloResponse)
	fullMethod := "/" + metaServiceName + "/Hello"
	if err := c.invokeMethod(ctx, fullMethod, "Hello", req, resp); err != nil {
		return HelloResponse{}, err
	}
	return *resp, nil
}

// invokeMethod is invoke (client_manual.go) generalized to an arbitrary
// full method path, so Hello (metaServiceName) can share it with the
// Workspace service's generated calls (serviceName).
func (c *Client) invokeMethod(ctx context.Context, fullMethod, name string, req, resp any) error {
	ctx = metadata.AppendToOutgoingContext(ctx, handleMetadataKey, c.handle)
	var trailer metadata.MD
	err := c.conn.Invoke(ctx, fullMethod, req, resp, grpc.CallContentSubtype(jsonCodecName), grpc.Trailer(&trailer))
	if err != nil {
		return decodeClientError(name, err, trailer)
	}
	return nil
}

// ServerOption configures NewServer.
type ServerOption func(*serverConfig)

type serverConfig struct {
	grpcOpts   []grpc.ServerOption
	serverHome func() string
}

// WithGRPCServerOptions passes extra grpc.ServerOption values through to
// grpc.NewServer (credentials, interceptors, ...).
func WithGRPCServerOptions(opts ...grpc.ServerOption) ServerOption {
	return func(c *serverConfig) { c.grpcOpts = append(c.grpcOpts, opts...) }
}

// WithServerHome overrides the value Hello reports as ServerHome; the
// default is os.UserHomeDir() at call time.
func WithServerHome(home func() string) ServerOption {
	return func(c *serverConfig) { c.serverHome = home }
}

// NewServer builds a *grpc.Server exposing ws as the root workspace
// handle (""), the Meta service's Hello, and the standard gRPC health
// service -- everything a Client (or `grpc_health_v1`'s own tooling)
// needs to talk to this process (CLIENT-SERVER.md, PR 1.1, build step 5).
// PR 1.3 adds non-root handles to resolve; until then, any other handle
// is a coded NotFound.
func NewServer(ws workspace.Workspace, opts ...ServerOption) *grpc.Server {
	cfg := &serverConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.serverHome == nil {
		cfg.serverHome = func() string {
			home, _ := os.UserHomeDir()
			return home
		}
	}

	s := grpc.NewServer(cfg.grpcOpts...)

	RegisterWorkspaceServer(s, func(ctx context.Context) (workspace.Workspace, error) {
		handle := handleFromContext(ctx)
		if handle != "" {
			return nil, status.Error(codes.NotFound, fmt.Sprintf("wsrpc: no workspace registered for handle %q", handle))
		}
		return ws, nil
	})
	s.RegisterService(&metaServiceDesc, &metaServer{workingDir: ws.WorkingDir, serverHome: cfg.serverHome})

	healthSrv := health.NewServer()
	healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	grpc_health_v1.RegisterHealthServer(s, healthSrv)

	return s
}

// handleFromContext reads the "sennit-handle" metadata key an incoming
// call carries (see Client.invoke), defaulting to the root handle "" when
// absent -- a caller talking to this server through anything other than
// this package's own Client (a hand-rolled grpc client, say) still resolves
// to the root workspace.
func handleFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(handleMetadataKey)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
