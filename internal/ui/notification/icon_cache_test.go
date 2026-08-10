package notification_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/ui/notification"
	"github.com/stretchr/testify/require"
)

// TestCacheIcon_WritesAndReturnsPath verifies the icon lands under
// <UserCacheDir>/braid/braid.png and the returned path is readable with
// matching content.
func TestCacheIcon_WritesAndReturnsPath(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	data := []byte("fake-png-bytes")
	path, err := notification.CacheIcon(data)
	require.NoError(t, err)
	require.Equal(t, "braid.png", filepath.Base(path))
	require.Equal(t, "braid", filepath.Base(filepath.Dir(path)))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, got)
}

// TestCacheIcon_SkipsRewriteWhenUnchanged confirms a second call with
// identical content doesn't touch the file (checked via mtime).
func TestCacheIcon_SkipsRewriteWhenUnchanged(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	data := []byte("fake-png-bytes")
	path, err := notification.CacheIcon(data)
	require.NoError(t, err)

	before, err := os.Stat(path)
	require.NoError(t, err)

	path2, err := notification.CacheIcon(data)
	require.NoError(t, err)
	require.Equal(t, path, path2)

	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "unchanged content must not rewrite the file")
}

// TestCacheIcon_RewritesOnContentChange covers the "icon changed between
// versions" case: a differing payload must overwrite the cached file rather
// than leaving the stale icon in place.
func TestCacheIcon_RewritesOnContentChange(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	path, err := notification.CacheIcon([]byte("old-icon"))
	require.NoError(t, err)

	path2, err := notification.CacheIcon([]byte("new-icon"))
	require.NoError(t, err)
	require.Equal(t, path, path2)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, []byte("new-icon"), got)
}
