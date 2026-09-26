package grpcws

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

// DefaultKeepaliveTime and DefaultKeepaliveTimeout are the gRPC keepalive
// ping interval and reply timeout NewServer and ClientDialOptions use
// unless overridden (CLIENT-SERVER.md, PR 1.3's "Уточнено ревью (п. 3)":
// a client's presence must be a live connection, confirmed by keepalive,
// not just an open RPC/stream). 30s/10s suits a long-lived local or
// SSH-bridged connection: idle enough that a healthy link never pings
// needlessly, short enough that a half-open connection (the remote end
// vanished without a FIN or RST, the common case over a dropped SSH
// tunnel) is found and closed well inside a session's own patience,
// rather than pinning that client's handles open forever -- see
// leaseManager, whose grace timer only starts once gRPC itself has ended
// the dead connection's stream.
const (
	DefaultKeepaliveTime    = 30 * time.Second
	DefaultKeepaliveTimeout = 10 * time.Second
)

// keepaliveMinTime derives the server's EnforcementPolicy.MinTime from
// its own ping interval: half of pingTime, so a client dialing with
// ClientDialOptions(pingTime, ...) -- the matching interval -- always
// clears it, while a client pinging much more often than the server
// itself does still gets GOAWAY "too_many_pings" instead of being served
// forever. Keeping this derived, rather than a fixed constant, is what
// lets a test shrink both sides together (short server Time, short
// client Time) without also having to keep a separate enforcement value
// in sync.
func keepaliveMinTime(pingTime time.Duration) time.Duration {
	return pingTime / 2
}

// serverKeepaliveOptions builds the grpc.ServerOption pair NewServer adds
// ahead of any caller-supplied WithGRPCServerOptions: ServerParameters
// pings an idle connection every pingTime and closes it if no ack arrives
// within pingTimeout; EnforcementPolicy accepts a client ping no more
// often than keepaliveMinTime(pingTime) and, per PermitWithoutStream,
// still accepts one with no active RPC/stream at all (a Client between
// calls, its own keepalive still ticking).
func serverKeepaliveOptions(pingTime, pingTimeout time.Duration) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    pingTime,
			Timeout: pingTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinTime(pingTime),
			PermitWithoutStream: true,
		}),
	}
}

// ClientDialOptions returns the grpc.DialOption values a wsrpc client
// should dial with: keepalive pings at pingTime/pingTimeout
// (PermitWithoutStream, so pings continue even between RPCs/streams --
// exactly the connection state a client with no call in flight is in most
// of the time) and the "json" content-subtype as the default call option,
// for a caller that dials this package's NewServer directly rather than
// going through Client (whose own invoke/openEventStream already select
// it per call). Later PRs (daemon attach) dial through this so every
// client keeps the same keepalive contract the server enforces.
//
// pingTime must be at least half of the server's own keepalive Time (see
// keepaliveMinTime) or the server answers with GOAWAY "too_many_pings"
// instead of serving calls; passing the same pingTime the server was
// built with (DefaultKeepaliveTime unless overridden) always satisfies
// this.
func ClientDialOptions(pingTime, pingTimeout time.Duration) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                pingTime,
			Timeout:             pingTimeout,
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(grpc.CallContentSubtype(jsonCodecName)),
	}
}

// DefaultClientDialOptions is ClientDialOptions at DefaultKeepaliveTime/
// DefaultKeepaliveTimeout, matching NewServer's own defaults.
func DefaultClientDialOptions() []grpc.DialOption {
	return ClientDialOptions(DefaultKeepaliveTime, DefaultKeepaliveTimeout)
}
