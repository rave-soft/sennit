package uicheck_test

import (
	"go/ast"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestDebug2(t *testing.T) {
	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedCompiledGoFiles,
		Dir:   "../../..",
		Tests: false,
	}
	pkgs, _ := packages.Load(cfg, "github.com/rave-soft/sennit/internal/ui/model")
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Body == nil || fn.Name.Name != "authenticateMCP" {
					return true
				}
				names := fn.Recv.List[0].Names
				t.Logf("found %s recvNames=%d", fn.Name.Name, len(names))
				recv := pkg.TypesInfo.Defs[names[0]]
				t.Logf("recv obj=%v nil=%v", recv, recv == nil)
				ast.Inspect(fn.Body, func(in ast.Node) bool {
					if lit, ok := in.(*ast.FuncLit); ok {
						t.Logf("funclit type=%v", pkg.TypesInfo.TypeOf(lit))
						ast.Inspect(lit.Body, func(u ast.Node) bool {
							if id, ok := u.(*ast.Ident); ok && id.Name == "m" {
								t.Logf("ident m uses=%v match=%v", pkg.TypesInfo.Uses[id], pkg.TypesInfo.Uses[id] == recv)
							}
							return true
						})
					}
					return true
				})
				return true
			})
		}
	}
}
