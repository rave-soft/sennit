package grpcws_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
	"github.com/rave-soft/sennit/internal/workspace/wsrpc/grpcws"
)

// wantUCMethods is every method wsrpc.MethodClasses classifies U or C --
// the set both WorkspaceServiceDesc and Client must cover exactly (no
// missing, no stray entries), per CLIENT-SERVER.md, PR 1.1's acceptance
// criteria.
func wantUCMethods() map[string]bool {
	want := map[string]bool{}
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.U || class == wsrpc.C {
			want[name] = true
		}
	}
	return want
}

// TestServiceDescCoversEveryUCMethod checks WorkspaceServiceDesc.Methods
// against wsrpc.MethodClasses in both directions: a U/C method the
// generator forgot, and a stray entry the classification table no longer
// has, both fail this test instead of surfacing later as a 404 or a
// dangling RPC nobody calls.
func TestServiceDescCoversEveryUCMethod(t *testing.T) {
	want := wantUCMethods()
	got := map[string]bool{}
	for _, md := range grpcws.WorkspaceServiceDesc.Methods {
		got[md.MethodName] = true
	}
	require.Equal(t, want, got)
}

// TestClientImplementsEveryUCMethod checks that *grpcws.Client has a
// method named after every U/C entry in wsrpc.MethodClasses. It doesn't
// check the reverse (Client also carries S/H/X methods and Hello, which
// isn't in MethodClasses at all) -- those are expected extras, not
// completeness gaps.
func TestClientImplementsEveryUCMethod(t *testing.T) {
	clientType := reflect.TypeOf((*grpcws.Client)(nil))
	for name := range wantUCMethods() {
		_, ok := clientType.MethodByName(name)
		require.True(t, ok, "grpcws.Client is missing method %s", name)
	}
}
