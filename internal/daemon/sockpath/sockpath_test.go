package sockpath

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"

	"github.com/stretchr/testify/require"
)

// TestPath_Deterministic pins that the same project directory always
// resolves to the same socket path across independent calls — a client
// dialing a daemon it never talked to before must be able to compute the
// exact same path the daemon listens on.
func TestPath_Deterministic(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	dir := t.TempDir()
	p1, err := Path(dir)
	require.NoError(t, err)
	p2, err := Path(dir)
	require.NoError(t, err)
	require.Equal(t, p1, p2)
	require.True(t, strings.HasSuffix(p1, ".sock"))
}

// TestPath_DiffersByDirectory pins that two distinct projects never
// collide on the same socket.
func TestPath_DiffersByDirectory(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	p1, err := Path(t.TempDir())
	require.NoError(t, err)
	p2, err := Path(t.TempDir())
	require.NoError(t, err)
	require.NotEqual(t, p1, p2)
}

// TestPath_RespectsXDGRuntimeDir pins that Path places the socket under
// XDG_RUNTIME_DIR/sennit rather than always falling back to os.TempDir.
func TestPath_RespectsXDGRuntimeDir(t *testing.T) {
	runtimeDir := testenv.ShortRuntimeDir(t)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	p, err := Path(t.TempDir())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(p, filepath.Join(runtimeDir, "sennit")+string(filepath.Separator)),
		"expected %q under %q", p, runtimeDir)
}

// TestPath_FallsBackWithoutXDGRuntimeDir pins that an unset
// XDG_RUNTIME_DIR still yields a usable path (os.TempDir()-based) rather
// than an error, on platforms where sockpath falls back to it.
func TestPath_FallsBackWithoutXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	// Redirect os.TempDir() itself into a throwaway directory so the
	// fallback branch (which mkdirs under it) never touches the real
	// system temp directory — on every OS os.TempDir() consults, hence
	// all four env vars.
	fallback := t.TempDir()
	t.Setenv("TMPDIR", fallback)
	t.Setenv("TMP", fallback)
	t.Setenv("TEMP", fallback)
	t.Setenv("LOCALAPPDATA", "")

	p, err := Path(t.TempDir())
	require.NoError(t, err)
	require.NotEmpty(t, p)
}

// TestPath_LengthLimit pins that a runtime directory long enough to push
// the resulting socket path over the platform's sockaddr_un.sun_path
// limit is rejected with an error naming the path and the environment
// variable to shorten it, rather than silently truncated or handed to
// net.Listen to fail unhelpfully.
func TestPath_LengthLimit(t *testing.T) {
	base := t.TempDir()
	// Pad well past even the more permissive 108-byte limit.
	longRuntimeDir := filepath.Join(base, strings.Repeat("x", 200))
	t.Setenv("XDG_RUNTIME_DIR", longRuntimeDir)

	_, err := Path(t.TempDir())
	require.Error(t, err)
	require.ErrorContains(t, err, "XDG_RUNTIME_DIR")
}
