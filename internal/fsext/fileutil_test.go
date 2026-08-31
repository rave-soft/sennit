package fsext

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// globAll collects every match from VisitGlobGitignoreAware, sorted by path
// so assertions do not depend on walk order. The collecting form used to
// live in fileutil.go; it was the last caller of a second walker that
// nothing in production ran, so these tests exercised gitignore and pattern
// behaviour on code the tools did not use. Result *ordering* is the glob
// tool's business, not this walker's, and is pinned in
// internal/agent/tools.
func globAll(t *testing.T, pattern, dir string) []string {
	t.Helper()

	// The walk is concurrent, so the collector needs its own lock — see
	// VisitGlobGitignoreAware's doc.
	var (
		mu      sync.Mutex
		matches []string
	)
	err := VisitGlobGitignoreAware(context.Background(), pattern, dir, func(path string, _ time.Time) {
		mu.Lock()
		defer mu.Unlock()
		matches = append(matches, path)
	})
	if err != nil {
		return nil
	}
	slices.Sort(matches)
	return matches
}

func globAllErr(t *testing.T, pattern, dir string) error {
	t.Helper()

	return VisitGlobGitignoreAware(context.Background(), pattern, dir, func(string, time.Time) {})
}

func TestGlobWithDoubleStar(t *testing.T) {
	t.Run("finds files matching pattern", func(t *testing.T) {
		testDir := t.TempDir()

		mainGo := filepath.Join(testDir, "src", "main.go")
		utilsGo := filepath.Join(testDir, "src", "utils.go")
		helperGo := filepath.Join(testDir, "pkg", "helper.go")
		readmeMd := filepath.Join(testDir, "README.md")

		for _, file := range []string{mainGo, utilsGo, helperGo, readmeMd} {
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
			require.NoError(t, os.WriteFile(file, []byte("test content"), 0o644))
		}

		matches := globAll(t, "**/main.go", testDir)

		require.Equal(t, matches, []string{mainGo})
	})

	t.Run("finds directories matching pattern", func(t *testing.T) {
		testDir := t.TempDir()

		srcDir := filepath.Join(testDir, "src")
		pkgDir := filepath.Join(testDir, "pkg")
		internalDir := filepath.Join(testDir, "internal")
		cmdDir := filepath.Join(testDir, "cmd")
		pkgFile := filepath.Join(testDir, "pkg.txt")

		for _, dir := range []string{srcDir, pkgDir, internalDir, cmdDir} {
			require.NoError(t, os.MkdirAll(dir, 0o755))
		}

		require.NoError(t, os.WriteFile(filepath.Join(srcDir, "main.go"), []byte("package main"), 0o644))
		require.NoError(t, os.WriteFile(pkgFile, []byte("test"), 0o644))

		matches := globAll(t, "pkg", testDir)

		require.Equal(t, matches, []string{pkgDir})
	})

	t.Run("finds nested directories with wildcard patterns", func(t *testing.T) {
		testDir := t.TempDir()

		srcPkgDir := filepath.Join(testDir, "src", "pkg")
		libPkgDir := filepath.Join(testDir, "lib", "pkg")
		mainPkgDir := filepath.Join(testDir, "pkg")
		otherDir := filepath.Join(testDir, "other")

		for _, dir := range []string{srcPkgDir, libPkgDir, mainPkgDir, otherDir} {
			require.NoError(t, os.MkdirAll(dir, 0o755))
		}

		matches := globAll(t, "**/pkg", testDir)

		var relativeMatches []string
		for _, match := range matches {
			rel, err := filepath.Rel(testDir, match)
			require.NoError(t, err)
			relativeMatches = append(relativeMatches, filepath.ToSlash(rel))
		}

		require.ElementsMatch(t, relativeMatches, []string{"pkg", "src/pkg", "lib/pkg"})
	})

	t.Run("finds directory contents with recursive patterns", func(t *testing.T) {
		testDir := t.TempDir()

		pkgDir := filepath.Join(testDir, "pkg")
		pkgFile1 := filepath.Join(pkgDir, "main.go")
		pkgFile2 := filepath.Join(pkgDir, "utils.go")
		pkgSubdir := filepath.Join(pkgDir, "internal")
		pkgSubfile := filepath.Join(pkgSubdir, "helper.go")

		require.NoError(t, os.MkdirAll(pkgSubdir, 0o755))

		for _, file := range []string{pkgFile1, pkgFile2, pkgSubfile} {
			require.NoError(t, os.WriteFile(file, []byte("package main"), 0o644))
		}

		matches := globAll(t, "pkg/**", testDir)

		var relativeMatches []string
		for _, match := range matches {
			rel, err := filepath.Rel(testDir, match)
			require.NoError(t, err)
			relativeMatches = append(relativeMatches, filepath.ToSlash(rel))
		}

		require.ElementsMatch(t, relativeMatches, []string{
			"pkg",
			"pkg/main.go",
			"pkg/utils.go",
			"pkg/internal",
			"pkg/internal/helper.go",
		})
	})

	t.Run("handles nested directory patterns", func(t *testing.T) {
		testDir := t.TempDir()

		file1 := filepath.Join(testDir, "a", "b", "c", "file1.txt")
		file2 := filepath.Join(testDir, "a", "b", "file2.txt")
		file3 := filepath.Join(testDir, "a", "file3.txt")
		file4 := filepath.Join(testDir, "file4.txt")

		for _, file := range []string{file1, file2, file3, file4} {
			require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
			require.NoError(t, os.WriteFile(file, []byte("test"), 0o644))
		}

		matches := globAll(t, "a/b/c/file1.txt", testDir)

		require.Equal(t, []string{file1}, matches)
	})

	t.Run("handles empty directory", func(t *testing.T) {
		testDir := t.TempDir()

		matches := globAll(t, "**", testDir)
		// Even empty directories should return the directory itself
		require.Equal(t, []string{testDir}, matches)
	})

	t.Run("handles non-existent search path", func(t *testing.T) {
		nonExistentDir := filepath.Join(t.TempDir(), "does", "not", "exist")

		require.Error(t, globAllErr(t, "**", nonExistentDir),
			"Should return error for non-existent search path")
	})

	t.Run("respects basic ignore patterns", func(t *testing.T) {
		testDir := t.TempDir()

		rootIgnore := filepath.Join(testDir, ".sennitignore")

		require.NoError(t, os.WriteFile(rootIgnore, []byte("*.tmp\nbackup/\n"), 0o644))

		goodFile := filepath.Join(testDir, "good.txt")
		require.NoError(t, os.WriteFile(goodFile, []byte("content"), 0o644))

		badFile := filepath.Join(testDir, "bad.tmp")
		require.NoError(t, os.WriteFile(badFile, []byte("temp content"), 0o644))

		goodDir := filepath.Join(testDir, "src")
		require.NoError(t, os.MkdirAll(goodDir, 0o755))

		ignoredDir := filepath.Join(testDir, "backup")
		require.NoError(t, os.MkdirAll(ignoredDir, 0o755))

		ignoredFileInDir := filepath.Join(testDir, "backup", "old.txt")
		require.NoError(t, os.WriteFile(ignoredFileInDir, []byte("old content"), 0o644))

		matches := globAll(t, "*.tmp", testDir)
		require.Empty(t, matches, "Expected no matches for '*.tmp' pattern (should be ignored)")

		matches = globAll(t, "backup", testDir)
		require.Empty(t, matches, "Expected no matches for 'backup' pattern (should be ignored)")

		matches = globAll(t, "*.txt", testDir)
		require.Equal(t, []string{goodFile}, matches)
	})

	t.Run("matches files and directories alike", func(t *testing.T) {
		testDir := t.TempDir()

		file := filepath.Join(testDir, "old.rs")
		require.NoError(t, os.WriteFile(file, []byte("old"), 0o644))

		dir := filepath.Join(testDir, "mid.rs")
		require.NoError(t, os.MkdirAll(dir, 0o755))

		other := filepath.Join(testDir, "new.rs")
		require.NoError(t, os.WriteFile(other, []byte("new"), 0o644))

		// A pattern that names a directory has to match it: nothing else
		// in the walk reports a directory, so dropping the match here
		// makes `glob "pkg"` indistinguishable from "there is no pkg".
		require.ElementsMatch(t, []string{file, dir, other}, globAll(t, "*.rs", testDir))
	})
}

func TestHasPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		prefix   string
		expected bool
	}{
		{
			name:     "sibling directory that merely starts with ..",
			path:     filepath.Join(string(filepath.Separator), "a", "..foo"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: true,
		},
		{
			name:     "nested path under a sibling that starts with ..",
			path:     filepath.Join(string(filepath.Separator), "a", "..foo", "bar"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: true,
		},
		{
			name:     "unrelated path is not a prefix match",
			path:     filepath.Join(string(filepath.Separator), "b"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: false,
		},
		{
			name:     "genuine parent-directory escape",
			path:     filepath.Join(string(filepath.Separator), "a", "..", "b"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: false,
		},
		{
			name:     "identical path",
			path:     filepath.Join(string(filepath.Separator), "a"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: true,
		},
		{
			name:     "normal nested path",
			path:     filepath.Join(string(filepath.Separator), "a", "b", "c"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: true,
		},
		{
			name:     "sibling with a shared textual prefix but different path",
			path:     filepath.Join(string(filepath.Separator), "ab"),
			prefix:   filepath.Join(string(filepath.Separator), "a"),
			expected: false,
		},
		{
			name:     "relative path resolves cleanly",
			path:     filepath.Join("root", "child", "..", "child", "file"),
			prefix:   "root",
			expected: true,
		},
		{
			name:     "relative path escaping root",
			path:     filepath.Join("root", "..", "sibling"),
			prefix:   "root",
			expected: false,
		},
		{
			name:     "absolute path is not within relative root",
			path:     filepath.Join(string(filepath.Separator), "root", "child"),
			prefix:   "root",
			expected: false,
		},
		{
			name:     "case follows platform path semantics",
			path:     filepath.Join(string(filepath.Separator), "root", "child"),
			prefix:   filepath.Join(string(filepath.Separator), "ROOT"),
			expected: runtime.GOOS == "windows",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.expected, HasPrefix(tt.path, tt.prefix))
		})
	}
}

func TestToWindowsLineEndings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		content     string
		expected    string
		expectedChg bool
	}{
		{
			name:        "pure LF converts fully",
			content:     "a\nb\nc",
			expected:    "a\r\nb\r\nc",
			expectedChg: true,
		},
		{
			name:        "mixed LF and CRLF is normalized to all CRLF",
			content:     "a\r\nb\nc",
			expected:    "a\r\nb\r\nc",
			expectedChg: true,
		},
		{
			name:        "already CRLF is unchanged",
			content:     "a\r\nb\r\nc",
			expected:    "a\r\nb\r\nc",
			expectedChg: false,
		},
		{
			name:        "empty string",
			content:     "",
			expected:    "",
			expectedChg: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, changed := ToWindowsLineEndings(tt.content)
			require.Equal(t, tt.expected, got)
			require.Equal(t, tt.expectedChg, changed)
			// The bool must faithfully report whether the content changed.
			require.Equal(t, got != tt.content, changed)

			// Idempotency: applying twice equals applying once.
			got2, changed2 := ToWindowsLineEndings(got)
			require.Equal(t, got, got2)
			require.False(t, changed2)
		})
	}
}

func TestLineEndingsRoundTrip(t *testing.T) {
	t.Parallel()

	// Simulates the edit-tool path: a CRLF file is normalized to LF, edited
	// (possibly introducing literal CRLF from the model output), and
	// converted back to CRLF. The round trip must not leave mixed endings.
	original := "line1\r\nline2\r\nline3"

	unix, _ := ToUnixLineEndings(original)
	require.Equal(t, "line1\nline2\nline3", unix)

	back, _ := ToWindowsLineEndings(unix)
	require.Equal(t, original, back)

	// Splice in LF-only content between the two conversions, as an edit tool
	// applying a model-provided replacement might.
	spliced := unix + "\nline4"
	backMixed, changed := ToWindowsLineEndings(spliced)
	require.True(t, changed)
	require.Equal(t, "line1\r\nline2\r\nline3\r\nline4", backMixed)
	require.NotContains(t, backMixed, "\n\n") // no stray bare LF left over
}

func TestSkipHidden(t *testing.T) {
	t.Parallel()

	t.Run("dot-prefixed base is always skipped", func(t *testing.T) {
		t.Parallel()
		require.True(t, SkipHidden(filepath.Join("proj", ".env")))
	})

	t.Run("ordinary path is not skipped", func(t *testing.T) {
		t.Parallel()
		require.False(t, SkipHidden(filepath.Join("proj", "src", "main.go")))
	})

	// commonIgnoredDirNames is the union of what used to be two
	// independently-maintained lists (SkipHidden's own commonIgnoredDirs
	// and ls.go's fastIgnoreDirs). Every name from both originals must
	// still be caught here, including the ones SkipHidden alone did not
	// carry before the merge — except the names the merge deliberately
	// dropped (see the "not skipped" case below).
	t.Run("skips every name from both merged lists", func(t *testing.T) {
		t.Parallel()
		names := []string{
			// Present in SkipHidden's own list before the merge.
			"node_modules", "vendor", "dist", "build", "target",
			".git", ".idea", ".vscode", "__pycache__", "bin", "obj",
			"out", "coverage", "bower_components", "jspm_packages",
			// Gained from ls.go's fastIgnoreDirs by the merge.
			".svn", ".hg", ".bzr", ".pytest_cache", ".cache", ".tmp",
			".Trash", ".Spotlight-V100", ".fseventsd",
		}
		for _, name := range names {
			p := filepath.Join("proj", name, "file.txt")
			require.True(t, SkipHidden(p), "expected %q to be skipped", p)
		}
	})

	// "generated" and "logs" are ordinary things to read and grep, and
	// "OrbStack"/".local"/".share" only make sense as ignores relative to
	// $HOME, not to every workspace path segment — see
	// commonIgnoredDirNames' own doc comment. All five were dropped from
	// the merged list rather than carried over; pin that they stay
	// visible.
	t.Run("does not skip names deliberately dropped from the merge", func(t *testing.T) {
		t.Parallel()
		names := []string{"generated", "logs", "OrbStack", ".local", ".share"}
		for _, name := range names {
			p := filepath.Join("proj", name, "file.txt")
			require.False(t, SkipHidden(p), "expected %q not to be skipped", p)
		}
	})
}
