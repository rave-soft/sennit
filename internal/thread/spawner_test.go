package thread

import (
	"context"
	"testing"

	"github.com/rave-soft/braid/internal/db"
	"github.com/stretchr/testify/require"
)

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

	require.True(t, handle.App().Store().Overrides().SkipPermissionRequests)
	require.True(t, handle.App().Permissions.SkipRequests())
}

// TestLocalSpawner_ConfinesWritesToTheWorktree pins the wiring end to end:
// a thread's workspace must report its worktree as a write boundary, not
// merely prefer it. Without this the file tools have nothing to refuse
// against, and an absolute path walks straight out of the thread and into
// the main checkout.
func TestLocalSpawner_ConfinesWritesToTheWorktree(t *testing.T) {
	repo := initRepo(t)
	spawner := NewLocalSpawner(nil, nil)
	t.Cleanup(func() { _ = spawner.Release(context.Background(), "") })

	handle, err := spawner.Spawn(t.Context(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = spawner.Release(context.Background(), handle.ID()) })

	perms := handle.App().Permissions
	require.NotEmpty(t, perms.ConfinedDir(),
		"a thread's workspace must be write-confined")
	require.Equal(t, repo, perms.ConfinedDir())
}
