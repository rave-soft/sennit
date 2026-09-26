package appws

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/stretchr/testify/require"
)

// newFilesTestWorkspace builds an AppWorkspace rooted at root with no
// database, LSP, MCP, or agent coordinator wired up - ListProjectFiles and
// AttachProjectFile (with an empty sessionID, skipping the file-tracker
// lookup) need none of that.
func newFilesTestWorkspace(t *testing.T, root string) *AppWorkspace {
	t.Helper()
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	store := configtest.NewStore(t, &config.Config{}, configtest.WithWorkingDir(root))
	a.SetConfigForTest(store)
	return NewAppWorkspace(a, store)
}

// TestListProjectFiles_MatchesTheOldClientSideListing pins the "@" file
// completion list to the same result the old internal/ui/completions.go
// produced by calling fsext.ListDirectory(".", ...) from the process's own
// cwd - now that the walk is rooted at the workspace's own WorkingDir()
// instead. See CLIENT-SERVER.md, "PR 0.6": in-process this must be an
// identical list, entries relative to root with a trailing separator on
// directories, ignored paths (via .gitignore) absent.
func TestListProjectFiles_MatchesTheOldClientSideListing(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureTree(t, root)

	w := newFilesTestWorkspace(t, root)

	files, err := w.ListProjectFiles(t.Context(), -1, -1)
	require.NoError(t, err)
	sort.Strings(files)

	require.Equal(t, []string{
		".gitignore",
		"regular.go",
		"sub/",
		"sub/inner.go",
	}, files)
}

// TestListProjectFiles_UnsetLimitsUseTodaysProjectDefault proves the 0/0
// "unset" convention resolves to the same default config.Config.
// CompletionsLimits() would have applied in-process, rather than an
// unlimited walk - see Workspace.ListProjectFiles's doc comment.
func TestListProjectFiles_UnsetLimitsUseTodaysProjectDefault(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// No git repository: applyEnvironmentDefaults would have clamped a
	// real Load() to depth 2 / 100 items. configtest.NewStore builds the
	// Config directly (bypassing Load), so set the same clamp explicitly
	// to stand in for it.
	depth, items := 2, 100
	cfg := &config.Config{Options: &config.Options{TUI: &config.TUIOptions{
		Completions: config.Completions{MaxDepth: &depth, MaxItems: &items},
	}}}

	nested := filepath.Join(root, "a", "b", "c")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "deep.txt"), []byte("x"), 0o644))

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	store := configtest.NewStore(t, cfg, configtest.WithWorkingDir(root))
	a.SetConfigForTest(store)
	w := NewAppWorkspace(a, store)

	files, err := w.ListProjectFiles(t.Context(), 0, 0)
	require.NoError(t, err)
	for _, f := range files {
		require.NotEqual(t, "a/b/c/deep.txt", f, "depth-2 default should not reach three levels down")
	}
}

// writeFixtureTree lays out a small project with one ignored directory,
// used by both the completions-equivalence test above and (via a copy) an
// intentional red check that resolving against the process cwd instead of
// the workspace root breaks it.
func writeFixtureTree(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "regular.go"), []byte("package main"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "inner.go"), []byte("package sub"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "ignored"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored", "skip.go"), []byte("package ignored"), 0o644))
}

// TestAttachProjectFile_ResolvesAgainstWorkspaceRootNotProcessCwd is the
// acceptance test's "red check on a copy under /tmp": AttachProjectFile
// must resolve a relative path against the workspace's own WorkingDir(),
// never the test binary's process cwd, which is this package's source
// directory and holds no such file. Constructing the workspace with a
// different root (rather than os.Chdir) keeps this safe to run alongside
// other parallel tests in the same binary.
func TestAttachProjectFile_ResolvesAgainstWorkspaceRootNotProcessCwd(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("hello"), 0o644))

	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NotEqual(t, root, cwd)
	_, statErr := os.Stat(filepath.Join(cwd, "notes.txt"))
	require.True(t, os.IsNotExist(statErr), "fixture leaked into the process cwd; test is not isolated")

	w := newFilesTestWorkspace(t, root)

	att, unchanged, err := w.AttachProjectFile(t.Context(), "", "notes.txt")
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Equal(t, []byte("hello"), att.Content)
}
