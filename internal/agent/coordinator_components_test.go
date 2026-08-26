package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

var componentTypeNames = map[string]string{
	"runtimeBuilder":      "builder",
	"turnDispatcher":      "dispatcher",
	"delegationFinalizer": "delegation",
}

func TestComponentTypesHaveNoSiblingPointerCycle(t *testing.T) {
	t.Parallel()
	components := map[string]reflect.Type{"builder": reflect.TypeOf(runtimeBuilder{}), "dispatcher": reflect.TypeOf(turnDispatcher{}), "delegation": reflect.TypeOf(delegationFinalizer{})}
	for owner, typ := range components {
		_, coordinator := typ.FieldByName("coordinator")
		require.Falsef(t, coordinator, "%s must not retain facade", owner)
		for i := range typ.NumField() {
			for sibling, siblingType := range components {
				if owner == sibling || typ.Field(i).Type != reflect.PointerTo(siblingType) {
					continue
				}
				for j := range siblingType.NumField() {
					require.NotEqualf(t, reflect.PointerTo(typ), siblingType.Field(j).Type, "component cycle: %s -> %s -> %s", owner, sibling, owner)
				}
			}
		}
	}
}

func TestCoordinatorWiringHasNoComponentCycle(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "coordinator.go"))
	require.NoError(t, err)
	require.NoError(t, componentWiringCycle(source))
}

func TestCoordinatorWiringGuardRejectsConcreteCallBoundaryCycle(t *testing.T) {
	require.Error(t, componentWiringCycle([]byte(`package agent
func wire(builder *runtimeBuilder, delegation *delegationFinalizer) {
 builder.buildTools(nil, config.Agent{}, false, delegation)
}`)))
}

func componentWiringCycle(source []byte) error {
	file, err := parser.ParseFile(token.NewFileSet(), "wiring.go", source, 0)
	if err != nil {
		return err
	}
	edges := map[string]map[string]bool{}
	addEdge := func(from, to string) {
		if from == "builder" && (to == "dispatcher" || to == "delegation") {
			edges[from] = map[string]bool{from: true}
			return
		}
		if from != "" && to != "" && from != to {
			if edges[from] == nil {
				edges[from] = map[string]bool{}
			}
			edges[from][to] = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		params := componentParameters(fn)
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.AssignStmt:
				for i, left := range n.Lhs {
					if i < len(n.Rhs) {
						addEdge(componentExpr(left, params), componentExpr(n.Rhs[i], params))
					}
				}
			case *ast.CallExpr:
				from := componentExpr(n.Fun, params)
				for _, arg := range n.Args {
					addEdge(from, componentExpr(arg, params))
				}
			}
			return true
		})
		return false
	})
	for owner := range edges {
		if componentReachable(edges, owner, owner, map[string]bool{}) {
			return fmt.Errorf("component wiring cycle through %s", owner)
		}
	}
	return nil
}

func componentParameters(fn *ast.FuncDecl) map[string]string {
	params := map[string]string{}
	if fn.Type.Params == nil {
		return params
	}
	for _, field := range fn.Type.Params.List {
		star, ok := field.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok {
			continue
		}
		component := componentTypeNames[ident.Name]
		for _, name := range field.Names {
			params[name.Name] = component
		}
	}
	return params
}

func componentExpr(expr ast.Expr, params map[string]string) string {
	if ident, ok := expr.(*ast.Ident); ok {
		return params[ident.Name]
	}
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if component := componentExpr(selector.X, params); component != "" {
		return component
	}
	if selector.Sel != nil {
		switch selector.Sel.Name {
		case "builder", "dispatcher", "delegation":
			return selector.Sel.Name
		}
	}
	return ""
}

func componentReachable(edges map[string]map[string]bool, start, current string, seen map[string]bool) bool {
	for next := range edges[current] {
		if next == start {
			return true
		}
		if !seen[next] {
			seen[next] = true
			if componentReachable(edges, start, next, seen) {
				return true
			}
		}
	}
	return false
}
