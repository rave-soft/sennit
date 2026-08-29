package model

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCommands_DoNotReadTheModelWhenTheyRun guards the rule the whole
// package is written to: a `func() tea.Msg` is a [tea.Cmd], and Bubble Tea
// runs it on its own goroutine at a time of its choosing — after Update
// has returned, and concurrently with the Update that handles the next
// message. Reading a field off the model from inside such a closure is a
// data race on the model, and it reads whatever the model happens to hold
// then rather than what it held when the command was built. Both failures
// are timing-dependent, so neither is reliably visible in a test.
//
// The rule is therefore a shape, not a behaviour: capture what the command
// needs into locals *before* returning the closure — `ws := m.com.Workspace`
// on the line above `return func() tea.Msg {` — and let the closure read
// only those. The package already follows it everywhere; this test is what
// keeps the next command from being the exception.
//
// Any method receiver counts, not just the UI model's `m`: since the state
// structs got their own methods, a command can now be built on a receiver
// named `s` or `c`, and reading that receiver inside the closure is the
// same race.
func TestCommands_DoNotReadTheModelWhenTheyRun(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
				continue
			}
			receiver := fn.Recv.List[0].Names[0].Name
			if receiver == "_" {
				continue
			}

			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.FuncLit)
				if !ok || !returnsTeaMsg(lit) {
					return true
				}
				ast.Inspect(lit.Body, func(n ast.Node) bool {
					selector, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					ident, ok := selector.X.(*ast.Ident)
					if !ok || ident.Name != receiver {
						return true
					}
					require.Fail(t,
						"a command reads the model when it runs",
						"%s: %s.%s is read inside a func() tea.Msg in %s. Capture it into a local before returning the closure — the closure runs on Bubble Tea's goroutine, concurrently with the next Update.",
						fset.Position(selector.Pos()), receiver, selector.Sel.Name, fn.Name.Name)
					return true
				})
				return true
			})
		}
	}
}

// returnsTeaMsg reports whether lit has the signature of a [tea.Cmd] body:
// no parameters used by the runtime, one result, and that result tea.Msg.
func returnsTeaMsg(lit *ast.FuncLit) bool {
	results := lit.Type.Results
	if results == nil || len(results.List) != 1 {
		return false
	}
	selector, ok := results.List[0].Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Msg" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "tea"
}
