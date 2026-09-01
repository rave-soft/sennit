package uicheck_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// ctxDetachOptOut marks a fresh root context created on purpose inside a
// function that has one already — work that must outlive the call it was
// started from, most often a terminal write on a path whose own context
// is the thing that just got cancelled.
const ctxDetachOptOut = "// ok: detached"

// TestNoFreshRootContextWhereOneWasGiven fails on context.Background() or
// context.TODO() inside a function that already receives a
// context.Context.
//
// A context is a chain: cancellation, deadlines and the values Sennit
// hangs off it (the session id, the delegation a permission prompt
// belongs to, the run id every provider log line carries) all reach a
// call because its caller passed them down. Starting a fresh root
// halfway breaks that chain silently — the code compiles, the call
// works, and what is lost only shows later: a tool that keeps running
// after the turn was cancelled, a permission prompt that cannot say
// which delegation raised it, a log line with no run to correlate it to.
// This is the one shape of that mistake a reader cannot spot by looking
// at a single line, because both halves — the parameter and the call —
// are correct on their own.
//
// It is not a ban on detaching. Terminal work genuinely has to outlive a
// cancelled context: internal/thread's failCreate writes a task's
// terminal status on a context that is very often the one that just
// died. Reach for context.WithoutCancel(ctx), which keeps the chain's
// values while dropping its cancellation, and where even that is wrong,
// mark the line "// ok: detached" and say what outlives what.
//
// Nested closures are attributed to themselves, not to the function
// around them: a goroutine started with its own lifetime is a different
// decision from a call that quietly drops its caller's.
//
// The lint side of this (forbidigo, .golangci.yml) forbids these calls
// outright inside internal/ui, where the TUI's own lifecycle context is
// the only right answer. This is the complement: everywhere else, the
// question is not whether a root context appears but whether one was
// already in hand.
func TestNoFreshRootContextWhereOneWasGiven(t *testing.T) {
	t.Parallel()

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedCompiledGoFiles,
		Dir:   "../../..",
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, moduleRoot+"/...")
	if err != nil {
		t.Fatalf("loading packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("packages failed to load; see errors above")
	}
	if len(pkgs) == 0 {
		t.Fatal("no packages matched; the pattern above is stale")
	}

	var findings []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			for _, fn := range ctxFunctions(pkg.TypesInfo, file) {
				for _, call := range freshRootCalls(pkg.TypesInfo, fn) {
					pos := pkg.Fset.Position(call.Pos())
					if hasOptOut(pkg, pos, ctxDetachOptOut) {
						continue
					}
					findings = append(findings, pos.String())
				}
			}
		}
	}

	if len(findings) > 0 {
		t.Errorf("a fresh root context where one was already given, in %d place(s):\n  %s\n\n"+
			"Pass the context you were given. To let work outlive a cancelled\n"+
			"context, use context.WithoutCancel(ctx) — it keeps the chain's values.\n"+
			"If a real root is right here, mark the line %q and say what outlives what.",
			len(findings), strings.Join(findings, "\n  "), ctxDetachOptOut)
	}
}

// ctxFunctions returns every function body in file whose own signature
// takes a context.Context — declarations and closures alike, each
// standing for itself.
func ctxFunctions(info *types.Info, file *ast.File) []*ast.BlockStmt {
	var out []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body != nil && takesContext(info, fn.Type) {
				out = append(out, fn.Body)
			}
		case *ast.FuncLit:
			if takesContext(info, fn.Type) {
				out = append(out, fn.Body)
			}
		}
		return true
	})
	return out
}

// takesContext reports whether sig has a context.Context parameter.
func takesContext(info *types.Info, sig *ast.FuncType) bool {
	if sig.Params == nil {
		return false
	}
	for _, param := range sig.Params.List {
		tv, ok := info.Types[param.Type]
		if !ok || tv.Type == nil {
			continue
		}
		if isContextType(tv.Type) {
			return true
		}
	}
	return false
}

// isContextType reports whether t is context.Context.
func isContextType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Name() == "Context" &&
		obj.Pkg() != nil && obj.Pkg().Path() == "context"
}

// freshRootCalls returns the context.Background()/context.TODO() calls
// made directly in body — not inside a nested closure, which stands for
// its own lifetime and is examined separately by ctxFunctions.
func freshRootCalls(info *types.Info, body *ast.BlockStmt) []*ast.CallExpr {
	var out []*ast.CallExpr
	ast.Inspect(body, func(n ast.Node) bool {
		if _, nested := n.(*ast.FuncLit); nested {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "Background" && sel.Sel.Name != "TODO") {
			return true
		}
		if fn, ok := info.Uses[sel.Sel].(*types.Func); ok &&
			fn.Pkg() != nil && fn.Pkg().Path() == "context" {
			out = append(out, call)
		}
		return true
	})
	return out
}

// TestCtxChainPredicatesAreExactAndEffective is the unit-level proof that
// the guard cannot regress into a vacuous pass. It runs the same two
// predicates the production check uses over a snippet that carries one
// example of every case, so a future edit that stops recognizing
// context.Context, or starts blaming a closure for the function around
// it, fails here rather than by quietly finding nothing.
func TestCtxChainPredicatesAreExactAndEffective(t *testing.T) {
	t.Parallel()

	const src = `package p

import "context"

type fake struct{}

func (fake) Background() context.Context { return nil }

func given(ctx context.Context) context.Context { return context.Background() }

func alsoGiven(name string, ctx context.Context) context.Context { return context.TODO() }

func none() context.Context { return context.Background() }

func closureOfItsOwn(ctx context.Context) func() context.Context {
	return func() context.Context { return context.Background() }
}

func closureGivenOne(ctx context.Context) func(context.Context) context.Context {
	return func(inner context.Context) context.Context { return context.Background() }
}

func lookalike(ctx context.Context, f fake) context.Context { return f.Background() }

func passesItOn(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }
`

	info, file := typeCheckSnippet(t, src)

	var found []string
	for _, body := range ctxFunctions(info, file) {
		for _, call := range freshRootCalls(info, body) {
			found = append(found, enclosingFuncName(file, call))
		}
	}
	sort.Strings(found)

	require.Equal(t, []string{"alsoGiven", "closureGivenOne", "given"}, found,
		"a fresh root is a finding exactly when the function it is written in was handed one")

	// Spelled out, so a regression says which half broke:
	//   given, alsoGiven      - the case this exists for; the parameter's
	//                           position does not matter
	//   closureGivenOne       - a closure with its own ctx is judged on it
	//   none                  - no context was given, so nothing was dropped
	//   closureOfItsOwn       - a closure with its own lifetime, not the
	//                           enclosing function's decision
	//   lookalike             - a Background method on some other type is
	//                           not context.Background
	//   passesItOn            - WithoutCancel is the sanctioned detach
	for _, name := range []string{"none", "closureOfItsOwn", "lookalike", "passesItOn"} {
		require.NotContains(t, found, name)
	}
}

// typeCheckSnippet parses and type-checks src as a standalone package,
// returning what the production predicates need from it.
func typeCheckSnippet(t *testing.T, src string) (*types.Info, *ast.File) {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	require.NoError(t, err)

	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Uses:  map[*ast.Ident]types.Object{},
		Defs:  map[*ast.Ident]types.Object{},
	}
	conf := types.Config{Importer: importer.Default()}
	_, err = conf.Check("p", fset, []*ast.File{file}, info)
	require.NoError(t, err)
	return info, file
}

// enclosingFuncName returns the name of the top-level function the call
// sits in, so a finding reads as a case name rather than an offset.
func enclosingFuncName(file *ast.File, call *ast.CallExpr) string {
	name := "?"
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		if call.Pos() >= fn.Body.Pos() && call.End() <= fn.Body.End() {
			name = fn.Name.Name
		}
		return true
	})
	return name
}
