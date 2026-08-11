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
