package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfigProductionDoesNotOwnProviderRuntime(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err)
			require.NotContains(t, []string{
				"github.com/rave-soft/sennit/internal/providerload",
				"github.com/rave-soft/sennit/internal/configruntime",
			}, path, "%s imports runtime orchestration package %s", name, path)
		}
	}
}

func TestProductionLoadsUseRuntimeFacade(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return walkErr
		}
		if filepath.Base(filepath.Dir(path)) == "configruntime" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (selector.Sel.Name != "Load" && selector.Sel.Name != "LoadWithProcessor") {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if ok && identifier.Name == "config" {
					t.Errorf("%s calls config.%s instead of configruntime", path, selector.Sel.Name)
				}
				return true
			})
		}
		return nil
	})
	require.NoError(t, err)
}
