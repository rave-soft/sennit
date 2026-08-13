package backend

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestDeleteWorkspaceShutsDownOnlyItsBackgroundShells(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	dataA, dataB := t.TempDir(), t.TempDir()
	clientA, clientB := uuid.New().String(), uuid.New().String()
	backend := New(context.Background(), nil, func() {})
	backend.SetCreateGrace(time.Minute)
	t.Cleanup(func() { drainBackend(t, backend) })
	first, _, err := backend.CreateWorkspace(proto.Workspace{Path: workspaceA, DataDir: dataA, ClientID: clientA})
	require.NoError(t, err)
	second, _, err := backend.CreateWorkspace(proto.Workspace{Path: workspaceB, DataDir: dataB, ClientID: clientB})
	require.NoError(t, err)
	firstJob, err := first.BackgroundShells.Start(t.Context(), workspaceA, nil, "sleep 5", "")
	require.NoError(t, err)
	secondJob, err := second.BackgroundShells.Start(t.Context(), workspaceB, nil, "sleep 5", "")
	require.NoError(t, err)
	require.NoError(t, backend.DeleteWorkspace(first.ID, clientA))
	require.True(t, firstJob.IsDone())
	require.False(t, secondJob.IsDone())
	require.NoError(t, backend.DeleteWorkspace(second.ID, clientB))
	require.True(t, secondJob.IsDone())
}
