package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// TestUIDoesNotLinkGRPC guards CLIENT-SERVER.md's PR 1.1 build step 6: the
// UI must keep talking to workspace.Workspace only through wsrpc's
// generated Request/Response types and Loopback, never a live network
// connection, so google.golang.org/grpc (and its own transitive tree, e.g.
// the health service) has no business in internal/ui's binary at all.
// wsrpc itself imports grpc (codec.go, client_manual.go, server.go); this
// package imports wsrpc for MethodClasses (see wsguard_test.go), so the
// only thing worth asserting is that internal/ui doesn't pull grpc in
// through some other, less obvious path.
func TestUIDoesNotLinkGRPC(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedImports | packages.NeedDeps | packages.NeedName}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/ui/...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)

	const grpcPrefix = "google.golang.org/grpc"
	seen := map[string]bool{}
	var walk func(pkg *packages.Package)
	walk = func(pkg *packages.Package) {
		if seen[pkg.PkgPath] {
			return
		}
		seen[pkg.PkgPath] = true
		if pkg.PkgPath == grpcPrefix || strings.HasPrefix(pkg.PkgPath, grpcPrefix+"/") {
			t.Errorf("internal/ui transitively depends on %s", pkg.PkgPath)
			return
		}
		for _, imp := range pkg.Imports {
			walk(imp)
		}
	}
	for _, pkg := range pkgs {
		require.Empty(t, pkg.Errors)
		walk(pkg)
	}
}
