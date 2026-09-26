package wsrpc

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPackageDoesNotImportInfrastructure mirrors
// internal/workspace/dependency_guard_test.go's
// TestDomainPackageDoesNotImportInfrastructure: wsrpc sits on the same
// UI-facing boundary as internal/workspace (internal/ui/model imports it,
// see wsguard_test.go's updateGoroutineGuardedMethods), so it must never
// import internal/db, internal/app, or internal/agent — see CLIENT-SERVER.md,
// PR 0.7's build step 1. It may import internal/workspace itself.
func TestPackageDoesNotImportInfrastructure(t *testing.T) {
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
				"github.com/rave-soft/sennit/internal/db",
				"github.com/rave-soft/sennit/internal/app",
				"github.com/rave-soft/sennit/internal/agent",
				"github.com/rave-soft/sennit/internal/thread",
				"github.com/rave-soft/sennit/internal/workspace/appws",
			}, path, "%s imports infrastructure package %s", name, path)
		}
	}
}
