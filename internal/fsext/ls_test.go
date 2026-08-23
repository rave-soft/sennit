package fsext

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListDirectory(t *testing.T) {
	tmp := t.TempDir()

	testFiles := map[string]string{
		"regular.txt":     "content",
		".hidden":         "hidden content",
		".gitignore":      ".*\n*.log\n",
		"subdir/file.go":  "package main",
		"subdir/.another": "more hidden",
		"build.log":       "build output",
	}

	for name, content := range testFiles {
		fp := filepath.Join(tmp, name)
		dir := filepath.Dir(fp)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(fp, []byte(content), 0o644))
	}

	t.Run("no limit", func(t *testing.T) {
		files, truncated, err := ListDirectory(tmp, nil, -1, -1)
		require.NoError(t, err)
		require.False(t, truncated)
		// The .gitignore has ".*" pattern which ignores hidden files anywhere
		// (like real git does), so subdir/.another is ignored.
		require.Len(t, files, 3)
		require.ElementsMatch(t, []string{
			"regular.txt",
			"subdir",
			"subdir/file.go",
		}, relPaths(t, files, tmp))
	})
	t.Run("limit", func(t *testing.T) {
		files, truncated, err := ListDirectory(tmp, nil, -1, 2)
		require.NoError(t, err)
		require.True(t, truncated)
		require.Len(t, files, 2)
	})
}

// TestListDirectorySkipsMergedFastIgnoreDirs pins a deliberate behavior
// change from reconciling fastIgnoreDirs with SkipHidden's own
// commonIgnoredDirs into one shared commonIgnoredDirNames list (see
// fileutil.go): these directory names were not in fastIgnoreDirs before
// the merge (and are not covered by commonIgnorePatterns' gitignore
// patterns either), so ListDirectory used to list their contents. It now
// skips them via the same O(1) fast path as node_modules or .git.
//
// "logs" and "generated" are deliberately not part of this list: they are
// ordinary directories to read and grep, so the merge dropped them rather
// than carrying them over — see the companion case below.
func TestListDirectorySkipsMergedFastIgnoreDirs(t *testing.T) {
	tmp := t.TempDir()

	for _, dir := range []string{"coverage", "bower_components", "jspm_packages"} {
		fp := filepath.Join(tmp, dir, "file.txt")
		require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o755))
		require.NoError(t, os.WriteFile(fp, []byte("x"), 0o644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "regular.txt"), []byte("x"), 0o644))

	files, truncated, err := ListDirectory(tmp, nil, -1, -1)
	require.NoError(t, err)
	require.False(t, truncated)
	require.Equal(t, []string{"regular.txt"}, relPaths(t, files, tmp))
}

// TestListDirectoryStillListsGeneratedAndLogs pins that dropping
// "generated" and "logs" from commonIgnoredDirNames (see that var's doc
// comment in fileutil.go) keeps both readable through ListDirectory:
// generated code is still code, and "check the logs" must not silently
// come back empty.
func TestListDirectoryStillListsGeneratedAndLogs(t *testing.T) {
	tmp := t.TempDir()

	for _, dir := range []string{"generated", "logs"} {
		fp := filepath.Join(tmp, dir, "file.txt")
		require.NoError(t, os.MkdirAll(filepath.Dir(fp), 0o755))
		require.NoError(t, os.WriteFile(fp, []byte("x"), 0o644))
	}

	files, truncated, err := ListDirectory(tmp, nil, -1, -1)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{
		"generated", "generated/file.txt",
		"logs", "logs/file.txt",
	}, relPaths(t, files, tmp))
}

func relPaths(tb testing.TB, in []string, base string) []string {
	tb.Helper()
	out := make([]string, 0, len(in))
	for _, p := range in {
		rel, err := filepath.Rel(base, p)
		require.NoError(tb, err)
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}
