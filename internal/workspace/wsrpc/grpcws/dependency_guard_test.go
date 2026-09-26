package grpcws

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
// internal/workspace/wsrpc/dependency_guard_test.go: grpcws is the gRPC
// transport for wsrpc's generated types (see this package's own doc
// comment on wsrpcPkgImportPath in the generator), so it inherits the same
// ban on internal/db, internal/app, internal/agent, internal/thread and
// internal/workspace/appws -- see CLIENT-SERVER.md, PR 1.1's build step 6.
// Unlike wsrpc itself, this package is allowed (indeed expected) to import
// google.golang.org/grpc; internal/ui/model's
// TestUIDoesNotLinkGRPC is what keeps that out of the UI's own closure.
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
