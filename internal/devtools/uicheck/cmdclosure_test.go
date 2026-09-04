package uicheck_test

import (
	"go/ast"
	"go/types"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestNoModelCaptureInCmdClosures fails when a tea.Cmd closure reads or
// writes the model it was built from.
//
// A tea.Cmd runs on its own goroutine, off the Update loop. A closure that
// captures the receiver therefore touches model state concurrently with
// Update — a data race that reproduces as a wrong file attached, a click
// landing on the wrong item, or a panic under load, all of which look like
// UI flakiness rather than a race. `internal/ui/AGENTS.md` states the
// convention; one review still found twelve violations of it, which is why
// it is checked here rather than remembered.
//
// The shape to use instead: read what you need inside Update, capture those
// values, and return the result as a message the model handles.
const cmdClosureOptOut = "// ok: no model access"

// allowedFromCmd are the receiver fields a command may reach through.
// com itself is set once when the model is built and never reassigned, so
// reading the field off the Update goroutine cannot race — and it is how a
// command is meant to reach the workspace, the styles and the lifecycle
// context in the first place. Everything else on the model is live state
// that Update mutates.
//
// That guarantee depends on com.Styles being swapped, not mutated in
// place: a theme switch used to overwrite *com.Styles's fields wholesale
// (`*m.com.Styles = ...`), which raced any command already holding that
// same pointer (see beginSessionLoad, which snapshots `styles :=
// m.com.Styles` before building one). setTheme now assigns a fresh
// pointer instead, so every snapshot a command captured stays exactly
// what it was — a stable copy of the old palette — and only new readers
// of the com.Styles field see the new one.
var allowedFromCmd = map[string]bool{
	"com": true,
}

func TestNoModelCaptureInCmdClosures(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedCompiledGoFiles,
		Dir:   "../../..",
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/ui/...")
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages failed to load; see errors above")
	}

	var findings []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
					return true
				}
				recv := receiverObject(pkg.TypesInfo, fn)
				if recv == nil {
					return true
				}
				ast.Inspect(fn.Body, func(inner ast.Node) bool {
					lit, ok := inner.(*ast.FuncLit)
					if !ok || !returnsTeaMsg(pkg.TypesInfo, lit) {
						return true
					}
					ast.Inspect(lit.Body, func(use ast.Node) bool {
						sel, ok := use.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						ident, ok := sel.X.(*ast.Ident)
						if !ok || pkg.TypesInfo.Uses[ident] != recv {
							return true
						}
						if allowedFromCmd[sel.Sel.Name] {
							return true
						}
						pos := pkg.Fset.Position(ident.Pos())
						if lineHas(pos.Filename, pos.Line, cmdClosureOptOut) {
							return true
						}
						findings = append(findings, pos.String()+" ("+fn.Name.Name+")")
						return true
					})
					return true
				})
				return true
			})
		}
	}

	if len(findings) > 0 {
		t.Errorf("tea.Cmd closure captures its model in %d place(s):\n  %s\n\n"+
			"Snapshot what you need inside Update and return a message instead;\n"+
			"see internal/ui/AGENTS.md. If a line genuinely cannot race (it only\n"+
			"reads an immutable value captured at construction), mark it %q.",
			len(findings), strings.Join(findings, "\n  "), cmdClosureOptOut)
	}
}

// receiverObject returns the method receiver's variable, or nil for an
// unnamed receiver.
func receiverObject(info *types.Info, fn *ast.FuncDecl) types.Object {
	names := fn.Recv.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return nil
	}
	return info.Defs[names[0]]
}

// returnsTeaMsg reports whether lit has the shape a tea.Cmd must have:
// no parameters, one tea.Msg result.
//
// Matched syntactically, on purpose. tea.Msg is declared as `type Msg =
// any`, so by the time the type checker is done the result type is a bare
// interface{} with no name left to test — asking types for "is this a
// tea.Msg" silently matches nothing and turns the whole check into a
// tautology. What the author wrote is the reliable signal here.
func returnsTeaMsg(_ *types.Info, lit *ast.FuncLit) bool {
	if lit.Type.Params != nil && len(lit.Type.Params.List) != 0 {
		return false
	}
	if lit.Type.Results == nil || len(lit.Type.Results.List) != 1 {
		return false
	}
	sel, ok := lit.Type.Results.List[0].Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Msg" {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	return ok && pkgIdent.Name == "tea"
}

// lineHas reports whether the given source line contains marker.
func lineHas(filename string, line int, marker string) bool {
	lines := readLines(filename)
	if line-1 >= len(lines) {
		return false
	}
	return strings.Contains(lines[line-1], marker)
}
