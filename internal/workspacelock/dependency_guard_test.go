package workspacelock

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWorkspaceLockIsALeaf pins what moving this code out of internal/db
// bought. The workspace lock is an flock on a project directory and has
// nothing to do with SQLite; while it lived in internal/db, taking it —
// which Bootstrap does before any database is opened — meant importing
// the database package and, through it, the driver. Anything that needs
// to know whether a project is already open by another sennit process
// had to pay for the whole storage layer to ask.
//
// Keeping this package a leaf is the property, so the test names the
// packages it must never reach rather than counting imports: brand, lock
// and version are what it legitimately uses, and every one of those is
// itself a leaf.
func TestWorkspaceLockIsALeaf(t *testing.T) {
	t.Parallel()

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
				"database/sql",
				"github.com/rave-soft/sennit/internal/db",
				"github.com/rave-soft/sennit/internal/config",
				"github.com/rave-soft/sennit/internal/app",
			}, path, "%s imports %s; the workspace lock must stay usable without the storage layer", name, path)
		}
	}
}
