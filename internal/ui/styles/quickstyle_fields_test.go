package styles

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// expectedStylesFields walks the Styles struct via reflection and returns
// the dotted path of every leaf field that quickStyle is responsible for
// populating. It descends into anonymous inline structs (Header, Dialog,
// Dialog.Sessions, ...) but treats named types from other packages
// (lipgloss.Style, textinput.Styles, ansi.StyleConfig, ...) as leaves,
// since those are always assigned as a whole compound literal rather than
// field-by-field. Unexported fields (just "rev") are excluded: it is
// stamped by Theme, not quickStyle.
func expectedStylesFields(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		path := f.Name
		if prefix != "" {
			path = prefix + "." + f.Name
		}
		if f.Type.Kind() == reflect.Struct && f.Type.Name() == "" {
			// Anonymous inline struct: descend.
			out = append(out, expectedStylesFields(f.Type, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}

// selectorPath renders a selector-expression chain rooted at the "s"
// identifier (the *Styles receiver every quickStyle builder takes) as a
// dotted field path, e.g. "s.Dialog.Sessions.DeletingTitle" ->
// "Dialog.Sessions.DeletingTitle". It reports false for anything not
// rooted at "s" (e.g. assignments to local variables).
func selectorPath(e ast.Expr) (string, bool) {
	switch t := e.(type) {
	case *ast.Ident:
		if t.Name == "s" {
			return "", true
		}
		return "", false
	case *ast.SelectorExpr:
		base, ok := selectorPath(t.X)
		if !ok {
			return "", false
		}
		if base == "" {
			return t.Sel.Name, true
		}
		return base + "." + t.Sel.Name, true
	default:
		return "", false
	}
}

// assignedStylesFields parses every quickstyle_*.go builder source file (plus
// quickstyle.go itself) and counts, for each dotted Styles field path, how
// many times it is the target of an "s.X.Y = ..." assignment across all of
// them combined.
func assignedStylesFields(t *testing.T) map[string]int {
	t.Helper()

	files, err := filepath.Glob("quickstyle*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	counts := map[string]int{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				path, ok := selectorPath(sel)
				if !ok {
					continue
				}
				counts[path]++
			}
			return true
		})
	}
	return counts
}

// TestQuickStyleFieldsAssignedOnce mechanically verifies that every field
// of Styles is assigned by exactly one quickStyle builder: it diffs the
// struct's leaf fields (via reflection) against the "s.X = ..." assignment
// targets found by parsing the builder source files (via go/ast). A field
// assigned zero times would silently keep its zero value; a field assigned
// more than once would silently take whichever builder ran last — both are
// the failure mode the split is supposed to rule out.
func TestQuickStyleFieldsAssignedOnce(t *testing.T) {
	expected := expectedStylesFields(reflect.TypeOf(Styles{}), "")
	sort.Strings(expected)

	actual := assignedStylesFields(t)

	var missing, multiple []string
	for _, path := range expected {
		switch actual[path] {
		case 0:
			missing = append(missing, path)
		case 1:
			// OK.
		default:
			multiple = append(multiple, path)
		}
	}

	if len(missing) > 0 {
		t.Errorf("%d Styles field(s) never assigned by any quickStyle builder:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(multiple) > 0 {
		t.Errorf("%d Styles field(s) assigned more than once across quickStyle builders:\n  %s",
			len(multiple), strings.Join(multiple, "\n  "))
	}

	// Also flag assignment targets that don't correspond to any known
	// Styles field at all -- e.g. a typo'd field name that the compiler
	// would normally have caught, were it not for embedding tricks.
	expectedSet := make(map[string]bool, len(expected))
	for _, p := range expected {
		expectedSet[p] = true
	}
	var unknown []string
	for path := range actual {
		if !expectedSet[path] {
			unknown = append(unknown, path)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("%d assignment(s) target unknown Styles field paths:\n  %s",
			len(unknown), strings.Join(unknown, "\n  "))
	}

	t.Logf("checked %d Styles leaf fields across %d assignment sites", len(expected), len(actual))
}
