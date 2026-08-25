package model

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionModelDoesNotOrchestrateSessionChanges(t *testing.T) {
	t.Parallel()

	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "session.go"))
	require.NoError(t, err)

	require.NotContains(t, string(source), `"github.com/rave-soft/sennit/internal/git"`)
	require.NotContains(t, string(source), `"github.com/rave-soft/sennit/internal/diff"`)
	require.NotContains(t, string(source), ".ListSessionHistory(")
	require.NotContains(t, string(source), ".UncommittedFiles(")
}
