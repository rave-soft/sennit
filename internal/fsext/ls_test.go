package fsext

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

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
	// "no limit" above has exactly 3 matches; a limit of exactly 3 must not
	// be reported as truncated (previously the walk stopped the instant it
	// collected the limit-th entry, which couldn't distinguish "exactly
	// limit entries total" from "there are more").
	t.Run("limit equal to total count", func(t *testing.T) {
		files, truncated, err := ListDirectory(tmp, nil, -1, 3)
		require.NoError(t, err)
		require.False(t, truncated, "limit exactly matching the total entry count must not report truncation")
		require.Len(t, files, 3)
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

func TestDirectoryListerDoesNotRetainWideTreeState(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	for i := range 2000 {
		dir := filepath.Join(tmp, fmt.Sprintf("dir-%04d", i))
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644))
	}
	dl := NewDirectoryLister(tmp)
	var count atomic.Int64
	incomplete, err := VisitDirectory(tmp, nil, 2, func(string) { count.Add(1) })
	require.NoError(t, err)
	require.False(t, incomplete)
	require.EqualValues(t, 4000, count.Load())
	value := reflect.ValueOf(dl).Elem()
	for i := range value.NumField() {
		kind := value.Field(i).Kind()
		require.NotEqual(t, reflect.Map, kind, "directory lister must not retain per-directory maps")
		require.NotEqual(t, reflect.Slice, kind, "directory lister must not retain per-directory slices")
	}
}

// TestListDirectory_DoesNotFollowSymlinkedDir pins ListDirectory to the
// same no-follow behavior as the glob path (globWithDoubleStar in
// fileutil.go): a symlinked subdirectory shows up as a leaf entry, and
// what is inside the real directory it points at is not listed a second
// time under the symlink. Before this fix ListDirectory passed
// fastwalk.Config{Follow: true}, so the same tree yielded different
// results depending on whether the agent used the list tool or glob.
func TestListDirectory_DoesNotFollowSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "inside.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(real, filepath.Join(tmp, "link")))

	files, truncated, err := ListDirectory(tmp, nil, -1, -1)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{
		"real", "real/inside.txt", "link",
	}, relPaths(t, files, tmp))
}

// TestListDirectory_SymlinkLoopDoesNotHang guards against the hang that
// following symlinks (fastwalk.Config{Follow: true}) invites: a directory
// that symlinks back to one of its own ancestors would otherwise send the
// walk in circles forever. With symlinks not followed, ListDirectory must
// return promptly and simply list the loop-forming symlink as a leaf.
func TestListDirectory_SymlinkLoopDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.Symlink(tmp, filepath.Join(sub, "loop")))

	done := make(chan struct{})
	var files []string
	var err error
	go func() {
		defer close(done)
		files, _, err = ListDirectory(tmp, nil, -1, -1)
	}()

	select {
	case <-done:
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"sub", "sub/loop"}, relPaths(t, files, tmp))
	case <-time.After(5 * time.Second):
		t.Fatal("ListDirectory hung on a symlink loop")
	}
}

// TestListDirectory_RootNamedLikeFastIgnoreDir pins that a walk root whose
// own basename is in fastIgnoreDirs (e.g. "vendor", "dist") still lists its
// contents: the ignore set exists to keep such directories out of
// *recursive* results, not to make them unlistable when asked for by name.
// Exercises both directoryLister.shouldIgnore (via ListDirectory) and
// directoryVisitState.shouldIgnore (via VisitDirectory), and confirms a
// recursive listing from the parent still omits the directory entirely.
func TestListDirectory_RootNamedLikeFastIgnoreDir(t *testing.T) {
	for _, name := range []string{"vendor", "dist"} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			target := filepath.Join(tmp, name)
			require.NoError(t, os.MkdirAll(target, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(target, "file.txt"), []byte("x"), 0o644))

			// ListDirectory (directoryLister.shouldIgnore) rooted directly
			// at the ignore-named directory must return its entries.
			files, truncated, err := ListDirectory(target, nil, -1, -1)
			require.NoError(t, err)
			require.False(t, truncated)
			require.Equal(t, []string{"file.txt"}, relPaths(t, files, target))

			// VisitDirectory (directoryVisitState.shouldIgnore) rooted the
			// same way must also see the entry.
			var visited []string
			var incomplete bool
			incomplete, err = VisitDirectory(target, nil, -1, func(p string) {
				visited = append(visited, p)
			})
			require.NoError(t, err)
			require.False(t, incomplete)
			require.Equal(t, []string{filepath.Join(target, "file.txt")}, visited)

			// Recursively listing from the parent must still omit the
			// directory's contents — the fast-ignore set still applies to
			// it as a descendant.
			parentFiles, _, err := ListDirectory(tmp, nil, -1, -1)
			require.NoError(t, err)
			require.Empty(t, parentFiles)
		})
	}
}

// chmodUnreadableDir creates a subdirectory under tmp that cannot be read,
// alongside a sibling file, and restores its permissions on cleanup so
// t.TempDir() can remove it. It skips on Windows (chmod does not restrict
// directory access there) and when running as root (root ignores the mode
// bit and would read the directory anyway).
func chmodUnreadableDir(t *testing.T, tmp string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not restrict directory access on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "visible.txt"), []byte("x"), 0o644))
	locked := filepath.Join(tmp, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "secret.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })
}

// TestListDirectory_UnreadableSubdirReportsIncomplete pins the fix for a
// directory listing reporting a partially-read tree as complete: fastwalk
// reports a ReadDir failure on "locked" as an error to the walk callback,
// which used to be swallowed with no trace. It must now surface as
// truncated=true, not as a clean, merely short listing.
func TestListDirectory_UnreadableSubdirReportsIncomplete(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	chmodUnreadableDir(t, tmp)

	files, truncated, err := ListDirectory(tmp, nil, -1, -1)
	require.NoError(t, err)
	require.True(t, truncated, "an unreadable subdirectory must make the listing report incompleteness")
	require.ElementsMatch(t, []string{"visible.txt", "locked"}, relPaths(t, files, tmp))
}

// TestListDirectory_FullyReadableTreeReportsComplete is the companion case:
// the ordinary, fully-readable tree must still report complete, unchanged
// from before the fix.
func TestListDirectory_FullyReadableTreeReportsComplete(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub", "file.txt"), []byte("x"), 0o644))

	files, truncated, err := ListDirectory(tmp, nil, -1, -1)
	require.NoError(t, err)
	require.False(t, truncated)
	require.ElementsMatch(t, []string{"sub", "sub/file.txt"}, relPaths(t, files, tmp))
}

// TestVisitDirectory_UnreadableSubdirReportsIncomplete is VisitDirectory's
// counterpart to TestListDirectory_UnreadableSubdirReportsIncomplete: it
// backs the ls tool (internal/agent/tools/ls.go), which surfaces the signal
// this asserts on to the model as LSResponseMetadata.Incomplete.
//
// VisitDirectory is built on filepath.Walk (unlike ListDirectory's
// fastwalk), which — for a directory it cannot read — calls the walk
// callback exactly once, with the ReadDir error and no entry for the
// directory itself, rather than fastwalk's "visit, then report the error
// separately" ordering. So "locked" itself is not among visited; the
// assertion that matters is that the walk still reports incompleteness
// instead of silently treating the subtree as empty.
func TestVisitDirectory_UnreadableSubdirReportsIncomplete(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	chmodUnreadableDir(t, tmp)

	var visited []string
	incomplete, err := VisitDirectory(tmp, nil, -1, func(p string) { visited = append(visited, p) })
	require.NoError(t, err)
	require.True(t, incomplete, "an unreadable subdirectory must make the visit report incompleteness")
	require.ElementsMatch(t, []string{"visible.txt"}, relPaths(t, visited, tmp))
}

// TestVisitDirectory_FullyReadableTreeReportsComplete is the companion case
// for VisitDirectory.
func TestVisitDirectory_FullyReadableTreeReportsComplete(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub", "file.txt"), []byte("x"), 0o644))

	var visited []string
	incomplete, err := VisitDirectory(tmp, nil, -1, func(p string) { visited = append(visited, p) })
	require.NoError(t, err)
	require.False(t, incomplete)
	require.ElementsMatch(t, []string{"sub", "sub/file.txt"}, relPaths(t, visited, tmp))
}

// TestVisitDirectory_FollowsSymlinkedRoot pins the fix for a root that is
// itself a directory symlink: filepath.Walk lstats its root, so a symlinked
// root used to report IsDir()==false and the walk returned zero entries
// with no error and incomplete==false — indistinguishable from an empty
// directory. VisitDirectory must instead list the target's contents, the
// same as ListDirectory (fastwalk stats its root) and a shell "ls" of the
// same path.
func TestVisitDirectory_FollowsSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "inside.txt"), []byte("x"), 0o644))
	link := filepath.Join(tmp, "link")
	require.NoError(t, os.Symlink(real, link))

	var visited []string
	incomplete, err := VisitDirectory(link, nil, -1, func(p string) { visited = append(visited, p) })
	require.NoError(t, err)
	require.False(t, incomplete)
	require.ElementsMatch(t, []string{"inside.txt"}, relPaths(t, visited, link))
}

// TestVisitDirectory_DoesNotFollowInteriorSymlink pins the other half of
// the same-sentence rule: a symlink found while descending an already-open
// root is reported as a leaf entry and not followed, the same as
// ListDirectory's fastwalk.Config{Follow: false}. What the symlink points
// at is not listed a second time underneath it.
func TestVisitDirectory_DoesNotFollowInteriorSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.Mkdir(real, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(real, "inside.txt"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(real, filepath.Join(tmp, "link")))

	var visited []string
	incomplete, err := VisitDirectory(tmp, nil, -1, func(p string) { visited = append(visited, p) })
	require.NoError(t, err)
	require.False(t, incomplete)
	require.ElementsMatch(t, []string{
		"real", "real/inside.txt", "link",
	}, relPaths(t, visited, tmp))
}

// TestVisitDirectory_SymlinkLoopDoesNotHang guards against the hang that
// following interior symlinks would invite: a directory that symlinks back
// to one of its own ancestors would otherwise send the walk in circles
// forever. With interior symlinks never followed, VisitDirectory must
// return promptly and list the loop-forming symlink as a leaf.
func TestVisitDirectory_SymlinkLoopDoesNotHang(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows")
	}
	t.Parallel()
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "sub")
	require.NoError(t, os.Mkdir(sub, 0o755))
	require.NoError(t, os.Symlink(tmp, filepath.Join(sub, "loop")))

	done := make(chan struct{})
	var visited []string
	var err error
	go func() {
		defer close(done)
		_, err = VisitDirectory(tmp, nil, -1, func(p string) { visited = append(visited, p) })
	}()

	select {
	case <-done:
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"sub", "sub/loop"}, relPaths(t, visited, tmp))
	case <-time.After(5 * time.Second):
		t.Fatal("VisitDirectory hung on a symlink loop")
	}
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
