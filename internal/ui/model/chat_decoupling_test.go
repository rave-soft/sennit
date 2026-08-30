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

// TestChatDoesNotDependOnTheScreen pins that chat.go names nothing declared
// elsewhere in this package.
//
// It is the property that makes the chat list a component rather than a
// part of the screen model, and it was not true until recently: chat.go
// carried the sidebar's scrollbar timer, which was the only place it
// mentioned *UI, and Chat had a method that returned a navigation type.
// Both were placement, not dependency — the timer moved to sidebar.go and
// the method now returns the list items it walks, leaving the mapping to
// navigation.
//
// Keeping it true is worth a test because nothing else would notice: the
// package compiles either way, and a single helper reached for from
// chat.go re-attaches the component to the screen silently.
func TestChatDoesNotDependOnTheScreen(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	chatFile, err := parser.ParseFile(fset, "chat.go", nil, 0)
	require.NoError(t, err)

	declaredElsewhere := map[string]string{}
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range files {
		if name == "chat.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			for _, declared := range topLevelNames(decl) {
				if declared == "_" {
					continue // a blank compile-time assertion declares nothing
				}
				declaredElsewhere[declared] = name
			}
		}
	}

	// Names chat.go declares itself shadow nothing: a collision would mean
	// two declarations of the same package-level name, which does not
	// compile.
	for _, decl := range chatFile.Decls {
		for _, declared := range topLevelNames(decl) {
			delete(declaredElsewhere, declared)
		}
	}

	ast.Inspect(chatFile, func(n ast.Node) bool {
		// A selector's field half (x.Foo) is not a package-level name,
		// and neither is a struct field or a key in a literal.
		switch node := n.(type) {
		case *ast.SelectorExpr:
			ast.Inspect(node.X, func(inner ast.Node) bool {
				checkIdent(t, fset, inner, declaredElsewhere)
				return true
			})
			return false
		case *ast.KeyValueExpr:
			ast.Inspect(node.Value, func(inner ast.Node) bool {
				checkIdent(t, fset, inner, declaredElsewhere)
				return true
			})
			return false
		}
		checkIdent(t, fset, n, declaredElsewhere)
		return true
	})
}

func checkIdent(t *testing.T, fset *token.FileSet, n ast.Node, declaredElsewhere map[string]string) {
	ident, ok := n.(*ast.Ident)
	if !ok {
		return
	}
	if where, found := declaredElsewhere[ident.Name]; found {
		require.Failf(t, "the chat component reached into the screen model",
			"%s: chat.go uses %q, declared in %s. The chat list is a component: it must not name anything else in internal/ui/model, so that moving it to its own package stays a mechanical change.",
			fset.Position(ident.Pos()), ident.Name, where)
	}
}

// topLevelNames returns the package-level names a declaration introduces.
func topLevelNames(decl ast.Decl) []string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil {
			// A method's name is not a package-level identifier.
			return nil
		}
		return []string{d.Name.Name}
	case *ast.GenDecl:
		var names []string
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, name := range s.Names {
					names = append(names, name.Name)
				}
			}
		}
		return names
	}
	return nil
}
