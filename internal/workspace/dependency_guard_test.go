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
	"golang.org/x/tools/go/packages"
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

// agentDepAllowList names packages under internal/agent/ that this package
// may legitimately keep in its transitive closure. It exists so a genuine
// future exception is a one-line, reviewable addition rather than a silent
// weakening of TestDomainPackageDoesNotDependOnAgentTransitively — as of
// this writing it is empty because nothing under internal/agent belongs on
// the wire between the UI and the Workspace contract.
var agentDepAllowList = map[string]string{}

// TestDomainPackageDoesNotDependOnAgentTransitively closes the gap in
// TestDomainPackageDoesNotImportInfrastructure above: that test only checks
// this package's own import lines, so a direct import of, say,
// internal/commands — which does not itself look like infrastructure —
// can still drag in internal/agent/tools/mcp (and, through it,
// internal/hooks and internal/shellconfig) several hops away. Since
// internal/ui imports this package for the Workspace interface and DTOs,
// any such transitive dependency is one internal/ui links too. Walk the
// full transitive closure with golang.org/x/tools/go/packages (equivalent
// to `go list -deps`) and fail on anything under internal/agent/ that
// isn't explicitly allow-listed above.
func TestDomainPackageDoesNotDependOnAgentTransitively(t *testing.T) {
	cfg := &packages.Config{Mode: packages.NeedImports | packages.NeedDeps | packages.NeedName}
	pkgs, err := packages.Load(cfg, "github.com/rave-soft/sennit/internal/workspace")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	require.Empty(t, pkgs[0].Errors)

	const agentPrefix = "github.com/rave-soft/sennit/internal/agent"
	seen := map[string]bool{}
	var walk func(pkg *packages.Package)
	walk = func(pkg *packages.Package) {
		if seen[pkg.PkgPath] {
			return
		}
		seen[pkg.PkgPath] = true
		if pkg.PkgPath == agentPrefix || strings.HasPrefix(pkg.PkgPath, agentPrefix+"/") {
			if reason, ok := agentDepAllowList[pkg.PkgPath]; ok {
				t.Logf("allowing %s: %s", pkg.PkgPath, reason)
			} else {
				t.Errorf("internal/workspace transitively depends on %s; internal/ui links this via the Workspace contract", pkg.PkgPath)
			}
		}
		for _, imp := range pkg.Imports {
			walk(imp)
		}
	}
	walk(pkgs[0])
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
