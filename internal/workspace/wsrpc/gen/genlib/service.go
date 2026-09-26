package genlib

import (
	"fmt"
	"strings"
)

// wsrpcPkgImportPath is where zz_generated_types.go's Request/Response DTOs
// live. renderService's own output lives in grpcws, a separate package
// precisely so those DTOs (and the rest of wsrpc) can be imported by
// internal/ui without dragging grpc along -- internal/ui must never link
// google.golang.org/grpc (see grpcws/dependency_guard_test.go and
// internal/ui/model/grpc_dependency_guard_test.go).
const wsrpcPkgImportPath = "github.com/rave-soft/sennit/internal/workspace/wsrpc"

// renderService builds grpcws/zz_generated_service.go: the grpc.ServiceDesc
// for the Workspace service, its server-side adapter (workspaceServer,
// RegisterWorkspaceServer, and one handler per method), and Client's
// generated methods for every entry in methods -- Generate only ever passes
// this the U subset (PR 1.4b: a class-C getter is answered from Client's
// own cache, hand-written in client_manual.go, not generated here).
//
// The literals serviceName, jsonCodecName and the helpers grpcStatusFromError
// / invoke referenced below are not spelled out here -- they are
// hand-written, package-level identifiers from grpcws/codec.go and
// grpcws/client_manual.go, in the same "package grpcws" this file is
// generated into (see CLIENT-SERVER.md, PR 1.1, build step 2).
func renderService(methods []*methodInfo) ([]byte, error) {
	imp := newImportSet()
	imp.register("context", "context")
	imp.register("log/slog", "slog")
	imp.register("google.golang.org/grpc", "grpc")
	wsAlias := imp.register(workspaceImportPath, "workspace")
	wireAlias := imp.register(wsrpcPkgImportPath, "wsrpc")

	var body strings.Builder
	body.WriteString("// WorkspaceServiceDesc, the server adapter and Client's generated\n")
	body.WriteString("// methods below are this package's gRPC transport for every U/C\n")
	body.WriteString("// workspace.Workspace method -- see codec.go and client_manual.go for\n")
	body.WriteString("// the hand-written parts they build on, and CLIENT-SERVER.md, PR 1.1.\n\n")

	writeServiceDescAndHandlers(&body, methods, wireAlias)
	writeServerAdapter(&body, methods, wsAlias, wireAlias)
	writeClientMethods(&body, methods, imp, wireAlias)

	src := generatedHeader + "package grpcws\n\n" + imp.importBlock() + "\n" + body.String()
	return gofumptFormat([]byte(src))
}

func writeServiceDescAndHandlers(body *strings.Builder, methods []*methodInfo, wireAlias string) {
	body.WriteString("// WorkspaceServer is the interface grpc.Server.RegisterService checks\n")
	body.WriteString("// workspaceServer against (google.golang.org/grpc requires\n")
	body.WriteString("// ServiceDesc.HandlerType to name an interface, not the concrete impl).\n")
	body.WriteString("type WorkspaceServer interface {\n")
	for _, m := range methods {
		fmt.Fprintf(body, "\t%s(ctx context.Context, req *%s.%sRequest) (*%s.%sResponse, error)\n", m.Name, wireAlias, m.Name, wireAlias, m.Name)
	}
	body.WriteString("}\n\n")

	body.WriteString("// WorkspaceServiceDesc is the Workspace gRPC service's description: one\n")
	body.WriteString("// unary RPC per U/C method in wsrpc.MethodClasses, named after the Go\n")
	body.WriteString("// method it wraps.\n")
	body.WriteString("var WorkspaceServiceDesc = grpc.ServiceDesc{\n")
	body.WriteString("\tServiceName: serviceName,\n")
	body.WriteString("\tHandlerType: (*WorkspaceServer)(nil),\n")
	body.WriteString("\tMethods: []grpc.MethodDesc{\n")
	for _, m := range methods {
		fmt.Fprintf(body, "\t\t{MethodName: %q, Handler: _Workspace_%s_Handler},\n", m.Name, m.Name)
	}
	body.WriteString("\t},\n")
	body.WriteString("\tStreams:  []grpc.StreamDesc{},\n")
	body.WriteString("\tMetadata: \"wsrpc/workspace\",\n")
	body.WriteString("}\n\n")

	for _, m := range methods {
		fmt.Fprintf(body, "func _Workspace_%s_Handler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {\n", m.Name)
		fmt.Fprintf(body, "\tin := new(%s.%sRequest)\n", wireAlias, m.Name)
		body.WriteString("\tif err := dec(in); err != nil {\n\t\treturn nil, err\n\t}\n")
		body.WriteString("\tif interceptor == nil {\n")
		fmt.Fprintf(body, "\t\treturn srv.(WorkspaceServer).%s(ctx, in)\n", m.Name)
		body.WriteString("\t}\n")
		fmt.Fprintf(body, "\tinfo := &grpc.UnaryServerInfo{Server: srv, FullMethod: \"/\" + serviceName + \"/%s\"}\n", m.Name)
		body.WriteString("\thandler := func(ctx context.Context, req any) (any, error) {\n")
		fmt.Fprintf(body, "\t\treturn srv.(WorkspaceServer).%s(ctx, req.(*%s.%sRequest))\n", m.Name, wireAlias, m.Name)
		body.WriteString("\t}\n")
		body.WriteString("\treturn interceptor(ctx, in, info, handler)\n")
		body.WriteString("}\n\n")
	}
}

// writeServerAdapter emits workspaceServer (the concrete type
// WorkspaceServiceDesc.HandlerType names), RegisterWorkspaceServer, and one
// method per entry in methods: resolve the target workspace.Workspace,
// call through to it, and map a non-nil error to a gRPC status carrying
// the encoded WireError (grpcStatusFromError, in client_manual.go).
func writeServerAdapter(body *strings.Builder, methods []*methodInfo, wsAlias, wireAlias string) {
	fmt.Fprintf(body, "// workspaceServer adapts a resolved %s.Workspace to WorkspaceServiceDesc.\n", wsAlias)
	fmt.Fprintf(body, "type workspaceServer struct {\n\tresolve func(ctx context.Context) (%s.Workspace, error)\n}\n\n", wsAlias)
	body.WriteString("// RegisterWorkspaceServer registers the Workspace service on s. resolve\n")
	body.WriteString("// picks the workspace.Workspace a call targets, reading the \"sennit-handle\"\n")
	body.WriteString("// metadata key set on the incoming ctx; PR 1.1 gives every caller the root\n")
	body.WriteString("// handle, so resolve here only ever needs to recognize \"\".\n")
	fmt.Fprintf(body, "func RegisterWorkspaceServer(s grpc.ServiceRegistrar, resolve func(ctx context.Context) (%s.Workspace, error)) {\n", wsAlias)
	body.WriteString("\ts.RegisterService(&WorkspaceServiceDesc, &workspaceServer{resolve: resolve})\n}\n\n")

	for _, m := range methods {
		fmt.Fprintf(body, "func (s *workspaceServer) %s(ctx context.Context, req *%s.%sRequest) (*%s.%sResponse, error) {\n", m.Name, wireAlias, m.Name, wireAlias, m.Name)
		body.WriteString("\tws, err := s.resolve(ctx)\n\tif err != nil {\n\t\treturn nil, grpcStatusFromError(ctx, err)\n\t}\n")

		resVars := make([]string, len(m.Results))
		for i := range m.Results {
			resVars[i] = fmt.Sprintf("res%d", i)
		}
		callArgs := buildCallArgsFrom(m, "req")
		if len(resVars) > 0 {
			fmt.Fprintf(body, "\t%s := ws.%s(%s)\n", strings.Join(resVars, ", "), m.Name, callArgs)
		} else {
			fmt.Fprintf(body, "\tws.%s(%s)\n", m.Name, callArgs)
		}
		if m.HasErr {
			errVar := resVars[len(resVars)-1]
			fmt.Fprintf(body, "\tif %s != nil {\n\t\treturn nil, grpcStatusFromError(ctx, %s)\n\t}\n", errVar, errVar)
		}
		fmt.Fprintf(body, "\treturn &%s.%sResponse{\n", wireAlias, m.Name)
		for i, f := range m.ResFields {
			fmt.Fprintf(body, "\t\t%s: %s,\n", f.GoName, resVars[i])
		}
		body.WriteString("\t}, nil\n}\n\n")
	}
}

// writeClientMethods emits one Client method per entry in methods,
// matching workspace.Workspace's own signature for it exactly (Client is a
// workspace.Workspace itself -- see client_manual.go's compile-time
// assertion). A method with no context.Context parameter gets one here,
// bounded by c.callTimeout, per CLIENT-SERVER.md's PR 1.1 build step. A
// method with no error result can't report a transport failure through its
// own signature, so it logs (slog.Warn) and returns the zero value instead.
// methods here is always the U subset (Generate filters C out before
// calling renderService); a class-C getter is a hand-written cache read on
// Client instead (client_manual.go).
func writeClientMethods(body *strings.Builder, methods []*methodInfo, imp *importSet, wireAlias string) {
	for _, m := range methods {
		hasCtx := false
		for _, p := range m.Params {
			if p.IsCtx {
				hasCtx = true
				break
			}
		}
		paramSig := buildParamSig(m, imp)
		resultSig := buildResultSig(m, imp)

		fmt.Fprintf(body, "// %s calls the Workspace service's %s RPC.\n", m.Name, m.Name)
		fmt.Fprintf(body, "func (c *Client) %s(%s) (%s) {\n", m.Name, paramSig, resultSig)

		if !hasCtx {
			body.WriteString("\tctx, cancel := context.WithTimeout(context.Background(), c.callTimeout)\n\tdefer cancel()\n")
		}

		fmt.Fprintf(body, "\twsrpcReq := &%s.%sRequest{\n", wireAlias, m.Name)
		for _, f := range m.ReqFields {
			fmt.Fprintf(body, "\t\t%s: %s,\n", f.GoName, f.ParamLocalName)
		}
		body.WriteString("\t}\n")
		fmt.Fprintf(body, "\twsrpcResp := new(%s.%sResponse)\n", wireAlias, m.Name)
		fmt.Fprintf(body, "\tif err := c.invoke(ctx, %q, wsrpcReq, wsrpcResp); err != nil {\n", m.Name)
		writeClientErrorReturn(body, m, imp)
		body.WriteString("\t}\n")
		writeClientSuccessReturn(body, m)
		body.WriteString("}\n\n")
	}
}

func writeClientErrorReturn(body *strings.Builder, m *methodInfo, imp *importSet) {
	if !m.HasErr {
		fmt.Fprintf(body, "\t\tslog.Warn(\"Wsrpc client call failed, returning zero value\", \"method\", %q, \"error\", err)\n", m.Name)
	}
	returns := make([]string, 0, len(m.ResFields)+1)
	for i, f := range m.ResFields {
		zeroVar := fmt.Sprintf("zero%d", i)
		fmt.Fprintf(body, "\t\tvar %s %s\n", zeroVar, typeString(f.Type, imp))
		returns = append(returns, zeroVar)
	}
	if m.HasErr {
		returns = append(returns, "err")
	}
	fmt.Fprintf(body, "\t\treturn %s\n", strings.Join(returns, ", "))
}

func writeClientSuccessReturn(body *strings.Builder, m *methodInfo) {
	parts := make([]string, 0, len(m.ResFields)+1)
	for _, f := range m.ResFields {
		parts = append(parts, "wsrpcResp."+f.GoName)
	}
	if m.HasErr {
		parts = append(parts, "nil")
	}
	if len(parts) == 0 {
		return
	}
	fmt.Fprintf(body, "\treturn %s\n", strings.Join(parts, ", "))
}

// buildCallArgsFrom is buildCallArgs (generate.go), generalized to read
// request fields off of an arbitrary variable name instead of always
// varDecodedReq -- the server adapter's generated methods read off "req",
// not off a loopback-style decoded copy.
func buildCallArgsFrom(m *methodInfo, source string) string {
	var parts []string
	for _, p := range m.Params {
		if p.IsCtx {
			parts = append(parts, "ctx")
			continue
		}
		arg := source + "." + exportName(p.LocalName)
		if p.Variadic {
			arg += "..."
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, ", ")
}
