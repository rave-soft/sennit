//go:build !windows

package hostaddr

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultHost_XDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	host := DefaultHost()

	require.True(t, strings.HasPrefix(host, "unix://"),
		"DefaultHost should return a unix:// URL, got %q", host)
	path := strings.TrimPrefix(host, "unix://")

	// The composed path may exceed maxUnixSocketPathLen and fall back
	// to /tmp; only assert containment when it did not. Recompose the
	// path under dir (rather than checking the returned path length,
	// which is short again after a /tmp fallback) to decide whether a
	// fallback happened. The socket is named braid-<uid>.sock.
	composed := filepath.Join(dir, filepath.Base(path))
	if len(composed) <= maxUnixSocketPathLen {
		require.True(t, strings.HasPrefix(path, dir),
			"socket path %q should live under %q", path, dir)
	}
	require.True(t, strings.HasSuffix(path, ".sock"),
		"socket path %q should end in .sock", path)
	require.Contains(t, filepath.Base(path), "braid",
		"socket filename should contain 'braid'")
}

func TestDefaultHost_FallbackTemp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	host := DefaultHost()

	require.True(t, strings.HasPrefix(host, "unix://"),
		"DefaultHost should return a unix:// URL, got %q", host)
	path := strings.TrimPrefix(host, "unix://")
	require.NotEmpty(t, path, "fallback socket path must be non-empty")
	require.True(t, strings.HasSuffix(path, ".sock"),
		"socket path %q should end in .sock", path)
	require.Contains(t, filepath.Base(path), "braid",
		"socket filename should contain 'braid'")
}
