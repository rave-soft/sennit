package grpcws

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

// jsonCodecName is the gRPC content-subtype this package registers and the
// one every wsrpc client call selects (grpc.CallContentSubtype). Build step
// 1 (CLIENT-SERVER.md, PR 1.1): no .proto is written for the Workspace
// service, so every message on the wire is a Request/Response struct from
// zz_generated_types.go, marshaled with encoding/json instead of protobuf.
const jsonCodecName = "json"

// serviceName is the Workspace service's fully-qualified gRPC name. No
// .proto backs it (see this package's doc comment), so the string is the
// only place the name is spelled out -- WorkspaceServiceDesc and every
// generated client call build the RPC's full method path from it.
const serviceName = "sennit.workspace.v1.Workspace"

// errorTrailerKey is the trailer metadata key a failed unary call's
// encoded workspace.WireError travels under. The "-bin" suffix makes gRPC
// treat the value as opaque bytes (base64 on the wire) rather than an
// ASCII header value, so a message containing non-ASCII text still
// survives — see grpcStatusFromError / decodeClientError.
const errorTrailerKey = "sennit-error-bin"

// jsonCodec is a google.golang.org/grpc/encoding.Codec (the v1 interface;
// see this package's doc comment on why no .proto and no CodecV2 buffer
// pooling is needed here) backed by encoding/json. Registering it under
// jsonCodecName lets both a client that sets
// grpc.CallContentSubtype("json") and the server, which picks a codec
// from the incoming request's content-subtype automatically, use it
// without any further wiring.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

func (jsonCodec) Name() string { return jsonCodecName }

func init() {
	encoding.RegisterCodec(jsonCodec{})
}
