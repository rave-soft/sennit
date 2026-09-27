package cmd

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/rave-soft/sennit/internal/transport"
	"github.com/rave-soft/sennit/internal/version"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// fakeMetaServer answers only the Meta service's Hello/Shutdown RPCs
// (grpcws.MetaServer), with a ProtocolVersion/BuildID this test controls
// -- everything checkRemoteVersion/connectRemoteWorkspace need to
// reconcile a version mismatch, and nothing else: there is no Workspace
// service registered here, so a call that goes further than Hello (e.g.
// client.Connect's own Snapshot) fails against this fake, which is fine
// for what these tests check -- both stop at (or right after)
// checkRemoteVersion.
type fakeMetaServer struct {
	protocolVersion int
	buildID         string
}

func (s *fakeMetaServer) Hello(context.Context, *grpcws.HelloRequest) (*grpcws.HelloResponse, error) {
	return &grpcws.HelloResponse{
		ProtocolVersion: s.protocolVersion,
		Version:         "fake",
		BuildID:         s.buildID,
		WorkingDir:      "/fake/project",
		ServerHome:      "/fake/home",
	}, nil
}

func (s *fakeMetaServer) Shutdown(context.Context, *grpcws.ShutdownRequest) (*grpcws.ShutdownResponse, error) {
	return &grpcws.ShutdownResponse{Accepted: false}, nil
}

// fakeMetaServiceDesc reimplements just enough of grpcws's own
// (unexported) metaServiceDesc to register a fakeMetaServer against a
// bare *grpc.Server: the wire method paths ("/sennit.workspace.v1.Meta/
// Hello" etc) are grpcws.Client's own hardcoded strings, so matching the
// service name here is what makes client.Hello reach this fake at all.
var fakeMetaServiceDesc = grpc.ServiceDesc{
	ServiceName: "sennit.workspace.v1.Meta",
	HandlerType: (*grpcws.MetaServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Hello",
			Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				req := new(grpcws.HelloRequest)
				if err := dec(req); err != nil {
					return nil, err
				}
				return srv.(grpcws.MetaServer).Hello(ctx, req)
			},
		},
		{
			MethodName: "Shutdown",
			Handler: func(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				req := new(grpcws.ShutdownRequest)
				if err := dec(req); err != nil {
					return nil, err
				}
				return srv.(grpcws.MetaServer).Shutdown(ctx, req)
			},
		},
	},
}

// startFakeMetaServer starts a bare *grpc.Server on a fresh unix socket,
// answering Hello with the given ProtocolVersion/BuildID -- enough to
// drive checkRemoteVersion/connectRemoteWorkspace's version reconciliation
// without a real daemon.
func startFakeMetaServer(t *testing.T, protocolVersion int, buildID string) (socketPath string) {
	t.Helper()
	socketPath = filepath.Join(testenv.ShortSocketDir(t), "fake-meta.sock")
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "unix", socketPath)
	require.NoError(t, err)

	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&fakeMetaServiceDesc, &fakeMetaServer{protocolVersion: protocolVersion, buildID: buildID})
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return socketPath
}

// TestConnectRemoteWorkspace_ProtocolMismatch checks CLIENT-SERVER.md's
// PR 3.1 test plan item: a ProtocolVersion mismatch surfaces from
// connectRemoteWorkspace as a hard error naming both versions, before
// ever reaching client.Connect (checkRemoteVersion runs first and
// returns early -- see connectRemoteWorkspace's own doc comment).
func TestConnectRemoteWorkspace_ProtocolMismatch(t *testing.T) {
	mismatched := grpcws.ProtocolVersion + 1
	socketPath := startFakeMetaServer(t, mismatched, "fake-build")

	target := transport.Target{Host: "fake-remote-host", Path: "/fake/project"}
	dialerOpts := transport.DialerOptions{Command: fakeSSHCommand(t, socketPath)}

	_, _, _, err := connectRemoteWorkspace(context.Background(), target, dialerOpts, "", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprint(mismatched), "error must name the daemon's protocol version")
	require.Contains(t, err.Error(), fmt.Sprint(grpcws.ProtocolVersion), "error must name this client's own protocol version")
}

// TestCheckRemoteVersion_BuildIDMismatchWarnsAndContinues checks the
// other half: a BuildID-only mismatch (ProtocolVersion agrees) is not an
// error at all -- checkRemoteVersion (the function connectRemoteWorkspace
// calls before proceeding to client.Connect) returns a warning naming
// both build IDs and a nil error, which is exactly what lets
// connectRemoteWorkspace continue past it.
func TestCheckRemoteVersion_BuildIDMismatchWarnsAndContinues(t *testing.T) {
	const remoteBuildID = "some-other-build-id"
	socketPath := startFakeMetaServer(t, grpcws.ProtocolVersion, remoteBuildID)

	target := transport.Target{Host: "fake-remote-host", Path: "/fake/project"}
	dialerOpts := transport.DialerOptions{Command: fakeSSHCommand(t, socketPath)}
	dialOpts := append(grpcws.DefaultClientDialOptions(),
		grpc.WithContextDialer(transport.SSHDialer(target, dialerOpts)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	conn, err := grpc.NewClient("passthrough:///"+target.String(), dialOpts...)
	require.NoError(t, err)
	defer conn.Close()

	client := grpcws.NewClient(conn)
	defer client.Shutdown()

	warning, err := checkRemoteVersion(context.Background(), client)
	require.NoError(t, err, "a BuildID-only mismatch must not be an error")
	require.Contains(t, warning, remoteBuildID)
	require.Contains(t, warning, version.Commit)
}
