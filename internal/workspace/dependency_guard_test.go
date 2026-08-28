package workspace

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDomainPackageDoesNotImportInfrastructure guards the contract/impl
// split this package went through: internal/ui imports this package for
// the Workspace interface and DTOs, so anything this package imports is
// pulled along transitively. The concrete AppWorkspace implementation
// (internal/workspace/appws) intentionally depends on internal/db,
// internal/app, internal/agent, and internal/thread — those belong there,
// not here. See internal/session/dependency_guard_test.go for the same
// pattern applied to session's model/store split.
//
// Unlike that test, this one does not forbid "sync" or "time": this
// package legitimately uses time.Time for DTO fields (see workspace.go,
// read_only_workspace.go) and has no coupling concern with either stdlib
// package the way it does with internal/db et al.
func TestDomainPackageDoesNotImportInfrastructure(t *testing.T) {
	matches, err := filepath.Glob("*.go")
	require.NoError(t, err)

	for _, name := range matches {
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		require.NoError(t, err)
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			require.NoError(t, err)
			require.NotContains(t, []string{
				"github.com/rave-soft/sennit/internal/db",
				"github.com/rave-soft/sennit/internal/app",
				"github.com/rave-soft/sennit/internal/agent",
				"github.com/rave-soft/sennit/internal/thread",
				"github.com/rave-soft/sennit/internal/workspace/appws",
			}, path, "%s imports infrastructure package %s", name, path)
		}
	}
}

func TestProductionFilesDoNotImportBubbleTea(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "bubbletea") {
			t.Errorf("%s imports bubbletea", entry.Name())
		}
	}
}
