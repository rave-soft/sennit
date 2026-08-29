package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// TestGlobTool_OrdersByModificationTime pins what the tool's own
// description promises the model — "sorted by modification time" — on the
// path the tool actually runs.
//
// It was true, then it was not: moving glob to keyset pagination made the
// page key the path, and nothing noticed for two reasons. The description
// was not part of the change, and the test that asserted newest-first was
// written against a collect-everything helper in fsext that the tool had
// stopped calling, so it kept passing while the tool it described did
// something else.
func TestGlobTool_OrdersByModificationTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Written oldest-name-first so an accidental return to path ordering
	// produces exactly the reverse of what is asserted.
	names := []string{"a.txt", "b.txt", "c.txt"}
	base := time.Now().Add(-time.Hour)
	for i, name := range names {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		stamp := base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, os.Chtimes(path, stamp, stamp))
	}

	response := runToolWith(t, NewGlobTool(dir, config.ToolGlob{}), t.Context(), GlobToolName,
		GlobParams{Pattern: "*.txt"})
	require.False(t, response.IsError, response.Content)

	got := strings.Split(strings.TrimSpace(response.Content), "\n")
	require.Equal(t, []string{
		filepath.Join(dir, "c.txt"),
		filepath.Join(dir, "b.txt"),
		filepath.Join(dir, "a.txt"),
	}, got, "newest first")
}

// TestGlobTool_FindsDirectories pins the other half of what the walk lost
// in the same change: a pattern naming a directory has to match it. With
// directories skipped, `glob "pkg"` returned nothing, which a caller
// cannot tell apart from there being no pkg at all.
func TestGlobTool_FindsDirectories(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg", "inner"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "inner", "f.go"), []byte("package inner"), 0o644))

	response := runToolWith(t, NewGlobTool(dir, config.ToolGlob{}), t.Context(), GlobToolName,
		GlobParams{Pattern: "pkg"})
	require.False(t, response.IsError, response.Content)
	require.Equal(t, filepath.Join(dir, "pkg"), strings.TrimSpace(response.Content))
}
