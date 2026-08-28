package stats

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

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
				"database/sql",
				"sync",
				"time",
				"github.com/rave-soft/sennit/internal/db",
				"github.com/rave-soft/sennit/internal/pubsub",
				"github.com/rave-soft/sennit/internal/stats/gather",
			}, path, "%s imports infrastructure package %s", name, path)
		}
	}
}
