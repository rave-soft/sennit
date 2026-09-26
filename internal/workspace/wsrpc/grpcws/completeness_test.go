package grpcws_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// wantUCMethods is every method wsrpc.MethodClasses classifies U or C --
// the set Client must cover exactly (no missing, no stray entries): every
// U method as a generated RPC method, every C method as a hand-written
// cache read (client_getters.go) -- see TestClientImplementsEveryUCMethod.
func wantUCMethods() map[string]bool {
	want := map[string]bool{}
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.U || class == wsrpc.C {
			want[name] = true
		}
	}
	return want
}

// wantUMethods is wantUCMethods narrowed to U alone -- the set
// WorkspaceServiceDesc must cover exactly, since PR 1.4b stopped
// generating an RPC for class C (a getter answered from Client's own
// cache, never a round trip -- see client_getters.go).
func wantUMethods() map[string]bool {
	want := map[string]bool{}
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.U {
			want[name] = true
		}
	}
	return want
}

// classCMethods is every method wsrpc.MethodClasses classifies C.
func classCMethods() map[string]bool {
	want := map[string]bool{}
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.C {
			want[name] = true
		}
	}
	return want
}

// serviceDescMethodNames collects grpcws.WorkspaceServiceDesc.Methods'
// names, shared by the two tests below.
func serviceDescMethodNames() map[string]bool {
	got := map[string]bool{}
	for _, md := range grpcws.WorkspaceServiceDesc.Methods {
		got[md.MethodName] = true
	}
	return got
}

// TestServiceDescCoversEveryUMethod checks WorkspaceServiceDesc.Methods
// against wsrpc.MethodClasses's U subset in both directions: a U method
// the generator forgot, and a stray entry the classification table no
// longer has, both fail this test instead of surfacing later as a 404 or a
// dangling RPC nobody calls.
func TestServiceDescCoversEveryUMethod(t *testing.T) {
	require.Equal(t, wantUMethods(), serviceDescMethodNames())
}

// TestServiceDescHasNoClassCMethods checks the other half of PR 1.4b's
// contract: a class-C getter must not be an RPC at all, not just absent
// from the U set above -- this fails loudly (naming the method) if a
// future edit to the generator, or to wsrpc.MethodClasses itself,
// resurrects one.
func TestServiceDescHasNoClassCMethods(t *testing.T) {
	got := serviceDescMethodNames()
	for name := range classCMethods() {
		require.False(t, got[name], "class-C method %s must not be a gRPC RPC (CLIENT-SERVER.md, PR 1.4b)", name)
	}
}

// TestClientImplementsEveryUCMethod checks that *grpcws.Client has a
// method named after every U/C entry in wsrpc.MethodClasses -- a U method
// generated as an RPC call, a C method hand-written as a cache read
// (client_getters.go). It doesn't check the reverse (Client also carries
// S/H/X methods and Hello, which isn't in MethodClasses at all) -- those
// are expected extras, not completeness gaps.
func TestClientImplementsEveryUCMethod(t *testing.T) {
	clientType := reflect.TypeOf((*grpcws.Client)(nil))
	for name := range wantUCMethods() {
		_, ok := clientType.MethodByName(name)
		require.True(t, ok, "grpcws.Client is missing method %s", name)
	}
}
