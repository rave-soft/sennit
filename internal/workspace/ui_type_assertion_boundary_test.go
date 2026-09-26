package workspace

import (
	"go/ast"
	"go/types"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// workspacePkgPath is this package's own import path, named once so every
// check below compares against the same string - a type assertion on a
// role interface declared here (Workspace, FrontendWorkspace,
// WorktreeController, FileServices, SessionChangePreparer, ...) is exactly
// the shape this guard forbids, wherever the interface itself came from
// (a bare workspace.Workspace, or common.Workspace, which is only a type
// alias for workspace.FrontendWorkspace and so resolves to the same
// *types.Named).
const workspacePkgPath = "github.com/rave-soft/sennit/internal/workspace"

// TestUIDoesNotTypeAssertWorkspace is PR 0.7c's review finding, generalized
// into a permanent guard: internal/ui/dialog/commands.go used to discover
// an optional capability (WorktreeState) with
// `com.Workspace.(interface{ WorktreeState() workspace.WorktreeState })`,
// and internal/ui/common and internal/ui/model did the same for
// PrepareSessionChanges (workspace.SessionChangePreparer). Both assertions
// worked against the in-process AppWorkspace but silently failed once the
// same value was wrapped in wsrpc.Loopback (and would fail the same way
// against a real gRPC client stub in PR 1.4): the capability did not
// disappear, but the frontend's only way of finding out about it did,
// with no compile error and no visible failure beyond a missing menu
// entry or a blank file list. The fix each time was the same: promote the
// capability onto Workspace itself (or one of its role interfaces) so
// every implementation, wrapper and future client is guaranteed to have
// it, and call it directly.
//
// This test makes that fix's logic a standing rule rather than a one-off
// finding: any *ast.TypeAssertExpr or *ast.TypeSwitchStmt anywhere under
// internal/ui, in a non-test file, whose asserted-from expression has a
// static type that is workspace.Workspace, common.Workspace,
// workspace.FrontendWorkspace, or any other named interface type declared
// in internal/workspace, is a violation - regardless of what the asserted
// type is. A capability a frontend needs belongs on the interface, not
// behind a probe only the in-process implementation happens to answer.
//
// Modeled on ui_config_boundary_test.go's TestUIDoesNotTouchConfigConfig:
// go/types-based rather than textual grep, for the same reason - a type
// assertion on a differently-named local variable, or one reached through
// several layers of assignment, greps invisibly but resolves to the same
// type under go/types.
func TestUIDoesNotTypeAssertWorkspace(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps | packages.NeedFiles,
	}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/ui/...")
	require.NoError(t, err)
	require.NotEmpty(t, pkgs, "expected at least one internal/ui package")

	var violations int
	check := func(filename string, info *types.Info, expr ast.Expr) {
		xt := info.TypeOf(expr)
		if xt == nil {
			return
		}
		named, ok := underlyingNamed(xt)
		if !ok {
			return
		}
		obj := named.Obj()
		if obj.Pkg() == nil {
			return
		}
		if _, isInterface := named.Underlying().(*types.Interface); !isInterface {
			return
		}
		if obj.Pkg().Path() != workspacePkgPath {
			return
		}
		violations++
		t.Errorf(
			"%s: type-asserts a value of type %s.%s; a capability the UI needs "+
				"belongs on the interface itself (see FileServices.PrepareSessionChanges "+
				"and WorktreeController.WorktreeState for the fix), not behind a probe "+
				"wsrpc.Loopback or a remote client cannot answer",
			filename, workspacePkgPath, obj.Name(),
		)
	}

	for _, pkg := range pkgs {
		for _, err := range pkg.Errors {
			t.Errorf("loading %s: %v", pkg.PkgPath, err)
		}
		for _, file := range pkg.Syntax {
			filename := pkg.Fset.Position(file.Package).Filename
			if isTestFile(filename) {
				continue
			}

			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.TypeAssertExpr:
					check(filename, pkg.TypesInfo, node.X)
				case *ast.TypeSwitchStmt:
					// The switch guard is either `x.(type)` directly or
					// `v := x.(type)` (an *ast.AssignStmt whose Rhs[0] is
					// the assert expr) - unwrap to the same *ast.TypeAssertExpr
					// either way.
					switch assign := node.Assign.(type) {
					case *ast.ExprStmt:
						if assert, ok := assign.X.(*ast.TypeAssertExpr); ok {
							check(filename, pkg.TypesInfo, assert.X)
						}
					case *ast.AssignStmt:
						for _, rhs := range assign.Rhs {
							if assert, ok := rhs.(*ast.TypeAssertExpr); ok {
								check(filename, pkg.TypesInfo, assert.X)
							}
						}
					}
				}
				return true
			})
		}
	}
	require.Zero(t, violations, "see individual t.Errorf calls above for each violation")
}
