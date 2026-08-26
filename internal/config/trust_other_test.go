//go:build !windows

package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTrustRequiresPrivateMarker pins the permission half of the check,
// which only Unix has: on Windows the mode bits are synthesized and say
// nothing about who can reach the file (see markerIsPrivate there).
func TestTrustRequiresPrivateMarker(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	project := t.TempDir()
	require.NoError(t, Trust(project))

	marker := trustPath(project)
	require.NoError(t, os.Chmod(marker, 0o644))
	require.False(t, IsTrusted(project), "a marker readable by other users must not grant trust")
	require.NoError(t, os.Chmod(marker, 0o600))
	require.True(t, IsTrusted(project))
}
