package grpcws

import (
	"context"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recoveryUnaryInterceptor and recoveryStreamInterceptor turn a panic
// raised anywhere below them in the interceptor chain -- a handler bug, a
// nil dereference in a workspace.Workspace implementation -- into a
// codes.Internal error instead of taking the whole process down with it.
// This matters far more for a daemon (CLIENT-SERVER.md, PR 2.1) than for
// the embedded mode a crashing TUI process already tears down on its own,
// but it costs nothing there either, so NewServer installs it
// unconditionally and ahead of every other interceptor (including
// lease's): only the outermost recover in a chain sees a panic raised by
// anything inside it, interceptors included.
func recoveryUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered panic in gRPC handler", "method", info.FullMethod, "panic", r)
			err = status.Errorf(codes.Internal, "internal error handling %s", info.FullMethod)
		}
	}()
	return handler(ctx, req)
}

func recoveryStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Recovered panic in gRPC stream handler", "method", info.FullMethod, "panic", r)
			err = status.Errorf(codes.Internal, "internal error handling %s", info.FullMethod)
		}
	}()
	return handler(srv, ss)
}
