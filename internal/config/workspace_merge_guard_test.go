package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// carriedAcrossWorkspaceMerge names every unexported field of Config,
// together with what puts it back after applyWorkspaceConfig's JSON round
// trip. That round trip is the merge — the workspace layer is applied by
// marshalling the config so far and re-loading it with the workspace bytes
// on top, which is how every other layer is merged too — and it drops
// everything encoding/json cannot see.
//
// Each entry is a claim that something restores the field. A new
// unexported field with no entry is the failure this catches: it would
// survive every test that does not load a workspace config, and silently
// reset for the users who have one.
var carriedAcrossWorkspaceMerge = map[string]string{
	"workingDir":              "restored by the setDefaults call after the merge",
	"jsonAgentsBlockDetected": "OR-ed onto the merged config explicitly",
	"resolvedEnv":             "not set until buildConfig calls populateRuntimeEnvironment, after applyWorkspaceConfig's merge and after cfg.Env is final, so the round trip never carries a value across it in the first place",
}

func TestConfigUnexportedFieldsSurviveTheWorkspaceMerge(t *testing.T) {
	t.Parallel()

	matches, err := filepath.Glob("*.go")
	require.NoError(t, err)

	found := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)

		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.TypeSpec)
			if !ok || spec.Name.Name != "Config" {
				return true
			}
			structType, ok := spec.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range structType.Fields.List {
				for _, ident := range field.Names {
					if ident.IsExported() {
						continue
					}
					found++
					require.Containsf(t, carriedAcrossWorkspaceMerge, ident.Name,
						"Config.%s is unexported, so applyWorkspaceConfig's JSON round trip drops it. "+
							"Restore it after the merge and add it to carriedAcrossWorkspaceMerge with how.", ident.Name)
				}
			}
			return false
		})
	}
	require.Equal(t, len(carriedAcrossWorkspaceMerge), found,
		"carriedAcrossWorkspaceMerge lists a field Config no longer has")
}
