package workspace

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
)

// TestUIGuardMethodsMatchWireClasses keeps internal/ui/model's
// updateGoroutineGuardedMethods table (wsguard_test.go) in lockstep with
// methodClasses here. The two cannot share one Go value: methodClasses
// lives in this package's own _test.go file, and _test.go symbols are only
// linked into their own package's test binary, so internal/ui/model cannot
// import it — the same reason internal/ui/model's own table cannot be
// imported back into this package's tests (see commit 59666e391, which
// forbade solving this by putting a test-only table in a production file
// just to make it importable both ways).
//
// Instead this test reads wsguard_test.go's source as text, parses out its
// map literal, and diffs the method-name set against the U/S/H entries in
// methodClasses. A method added to one table without the other fails here
// with the exact name that drifted, in either direction.
func TestUIGuardMethodsMatchWireClasses(t *testing.T) {
	t.Parallel()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the sibling ui/model package")
	}
	guardFile := filepath.Join(filepath.Dir(thisFile), "..", "ui", "model", "wsguard_test.go")

	guarded, err := parseGuardedMethodNames(guardFile)
	if err != nil {
		t.Fatalf("parsing %s: %v", guardFile, err)
	}
	if len(guarded) == 0 {
		t.Fatalf("found no entries in updateGoroutineGuardedMethods in %s; parsing must have failed silently", guardFile)
	}

	wantUSH := make(map[string]bool)
	for name, class := range methodClasses {
		if class == classCachedGetter || class == classClientLocal {
			continue
		}
		wantUSH[name] = true
	}

	var missingFromGuard, extraInGuard []string
	for name := range wantUSH {
		if !guarded[name] {
			missingFromGuard = append(missingFromGuard, name)
		}
	}
	for name := range guarded {
		if !wantUSH[name] {
			extraInGuard = append(extraInGuard, name)
		}
	}
	sort.Strings(missingFromGuard)
	sort.Strings(extraInGuard)

	for _, name := range missingFromGuard {
		t.Errorf("%s is class %s in methodClasses but missing from updateGoroutineGuardedMethods in %s; "+
			"the UI test guard would not catch a synchronous call to it", name, methodClasses[name], guardFile)
	}
	for _, name := range extraInGuard {
		t.Errorf("%s is in updateGoroutineGuardedMethods (%s) but is not a U/S/H method in methodClasses; "+
			"either Workspace lost the method or the guard table is stale", name, guardFile)
	}
}

// parseGuardedMethodNames extracts the string keys of the
// updateGoroutineGuardedMethods map[string]bool literal from path, without
// importing the package it lives in (see the test's doc comment for why).
func parseGuardedMethodNames(path string) (map[string]bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, err
	}

	names := make(map[string]bool)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			nameMatches := false
			for _, ident := range vspec.Names {
				if ident.Name == "updateGoroutineGuardedMethods" {
					nameMatches = true
				}
			}
			if !nameMatches {
				continue
			}
			for _, value := range vspec.Values {
				lit, ok := value.(*ast.CompositeLit)
				if !ok {
					continue
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						continue
					}
					unquoted, err := strconv.Unquote(key.Value)
					if err != nil {
						return nil, err
					}
					names[unquoted] = true
				}
			}
		}
	}
	return names, nil
}
