package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrustMarksProjectTrusted(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	project := t.TempDir()

	require.False(t, IsTrusted(project))
	require.NoError(t, Trust(project))
	require.True(t, IsTrusted(project))
}

func TestTrustRejectsNonRegularMarker(t *testing.T) {
	globalDir := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", globalDir)
	project := t.TempDir()
	marker := trustPath(project)
	require.NoError(t, os.MkdirAll(filepath.Dir(marker), 0o700))
	require.NoError(t, os.Mkdir(marker, 0o700))

	require.False(t, IsTrusted(project))
	require.Error(t, Trust(project))
}
