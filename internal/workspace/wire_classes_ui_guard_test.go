// Package workspace_test (external, not workspace's own internal test
// package): this file imports wsrpc, which imports workspace itself, and
// an internal ("package workspace") test file importing anything that
// imports workspace back is a cycle Go's toolchain refuses outright ("import
// cycle not allowed in test") — the external test package is the standard
// way around that, and this file needs nothing unexported from workspace
// anyway.
package workspace_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/rave-soft/sennit/internal/workspace/wsrpc"
)

// TestUIGuardHasWrapperForEveryWireMethod keeps internal/ui/model's
// updateGoroutineGuard (wsguard_test.go) honest: every U/S/H method in
// wsrpc.MethodClasses must have an explicit guarded wrapper method there,
// or a synchronous call to it on the Update goroutine would silently fall
// through updateGoroutineGuard's embedded workspace.Workspace and never
// trip g.check.
//
// This used to also diff wsguard_test.go's updateGoroutineGuardedMethods
// map against methodClasses in the other direction (an entry in the guard
// table that methodClasses didn't recognize). That table is now computed
// from wsrpc.MethodClasses directly (see wsguard_test.go), so it can no
// longer drift from this list on its own; the one thing that still isn't
// guaranteed by the compiler is whether a wrapper method exists at all for
// each name in that computed set, which is what this test checks instead.
// The reverse case — a wrapper naming a method Workspace no longer has —
// is already caught by compilation: the wrapper's body calls
// g.Workspace.<Method>(...), and that fails to build the moment the
// embedded interface drops the method.
func TestUIGuardHasWrapperForEveryWireMethod(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the sibling ui/model package")
	}
	guardFile := filepath.Join(filepath.Dir(thisFile), "..", "ui", "model", "wsguard_test.go")

	wrapped, err := parseGuardWrapperMethods(guardFile)
	if err != nil {
		t.Fatalf("parsing %s: %v", guardFile, err)
	}
	if len(wrapped) == 0 {
		t.Fatalf("found no *updateGoroutineGuard method declarations in %s; parsing must have failed silently", guardFile)
	}

	var missing []string
	for name, class := range wsrpc.MethodClasses {
		if class == wsrpc.C || class == wsrpc.X {
			continue
		}
		if !wrapped[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	for _, name := range missing {
		t.Errorf("%s is class %s in wsrpc.MethodClasses but has no *updateGoroutineGuard wrapper method in %s; "+
			"the UI test guard would not catch a synchronous call to it", name, wsrpc.MethodClasses[name], guardFile)
	}
}

// parseGuardWrapperMethods extracts the method names declared on
// *updateGoroutineGuard in path, without importing the package it lives in
// (internal/ui/model cannot import this package's _test.go table, and this
// package cannot import ui/model's — see commit 59666e391).
func parseGuardWrapperMethods(path string) (map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	names := make(map[string]bool)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recvType := fn.Recv.List[0].Type
		star, ok := recvType.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "updateGoroutineGuard" {
			continue
		}
		names[fn.Name.Name] = true
	}
	return names, nil
}
