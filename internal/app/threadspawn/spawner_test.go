package threadspawn

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/stretchr/testify/require"
)

// TestWorkspaceAdaptersAreOwnershipScoped prevents reintroducing global
// canonicalization. A globally retained adapter also retains its App and all
// of the App's services after LocalSpawner.Release or backend release. Each
// handle/spawner instead owns the one adapter whose identity it must keep
// stable for its own lifetime.
func TestWorkspaceAdaptersAreOwnershipScoped(t *testing.T) {
	a := app.NewForTest(context.Background())
	t.Cleanup(a.ShutdownForTest)

	first := NewAppWorkspaceAdapter(a)
	second := NewAppWorkspaceAdapter(a)
	require.NotSame(t, first, second, "adapters must not be retained in a process-wide cache")

	h := &localHandle{id: "thread", app: a, workspace: first}
	require.Same(t, first, h.Workspace(), "a handle must keep its own adapter stable")
	require.Same(t, first, h.Workspace(), "repeated Workspace calls must preserve identity")
}

func TestLocalSpawnerInheritsParentYOLO(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { db.ResetPool() })

	spawner := NewLocalSpawner(nil, func() bool { return true })
	handle, err := spawner.Spawn(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })

	// localHandle exposes App() for the test.
	lh, ok := handle.(*localHandle)
	require.True(t, ok)
	require.True(t, lh.app.Store().Overrides().SkipPermissionRequests)
	require.True(t, lh.app.Permissions().SkipRequests())
}

func TestLocalSpawnerConfinesWritesToWorktree(t *testing.T) {
	repo := initRepo(t)
	spawner := NewLocalSpawner(nil, nil)

	handle, err := spawner.Spawn(t.Context(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })

	local, ok := handle.(*localHandle)
	require.True(t, ok)
	require.Equal(t, repo, local.app.Permissions().ConfinedDir())
}
