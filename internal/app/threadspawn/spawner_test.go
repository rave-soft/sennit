package threadspawn

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/skills"
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

	spawner := NewLocalSpawner(nil, nil, func() bool { return true }, nil)
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
	spawner := NewLocalSpawner(nil, nil, nil, nil)

	handle, err := spawner.Spawn(t.Context(), repo)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })

	local, ok := handle.(*localHandle)
	require.True(t, ok)
	require.Equal(t, repo, local.app.Permissions().ConfinedDir())
}

// A thread runs in a git worktree, which has no .sennit/skills of its own,
// and must not go looking for one in the main checkout. The parent hands
// its skills down at spawn instead.
func TestLocalSpawnerInheritsParentSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { db.ResetPool() })

	parentSkill := &skills.Skill{
		Name:         "parent-skill",
		Description:  "Handed down to every thread.",
		Instructions: "Parent instructions.",
	}
	spawner := NewLocalSpawner(nil, func() []*skills.Skill { return []*skills.Skill{parentSkill} }, nil, nil)

	handle, err := spawner.Spawn(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })

	lh, ok := handle.(*localHandle)
	require.True(t, ok)

	var got *skills.Skill
	for _, s := range lh.app.Skills.ActiveSkills() {
		if s.Name == "parent-skill" {
			got = s
		}
	}
	require.NotNil(t, got, "a spawned thread must receive the parent workspace's skills")
	require.Equal(t, "Parent instructions.", got.Instructions)

	require.Len(t, spawner.Apps(), 1, "a live thread must be reachable for skill updates")
}

// The point of inheriting skills would be lost if the copy froze at spawn:
// editing a SKILL.md while threads are running must reach the threads that
// are already working, not only the next one to start.
func TestForwardSkillsToThreadsPushesUpdateToLiveThread(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { db.ResetPool() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	parent := app.NewForTest(ctx)
	t.Cleanup(parent.ShutdownForTest)
	parent.Skills = skills.NewManager(nil, nil, nil)

	spawner := NewLocalSpawner(nil, nil, nil, nil)
	handle, err := spawner.Spawn(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })
	lh, ok := handle.(*localHandle)
	require.True(t, ok)

	go forwardSkillsToThreads(ctx, parent, spawner)

	edited := &skills.Skill{
		Name:         "parent-skill",
		Description:  "Edited while the thread was running.",
		Instructions: "Edited instructions.",
	}
	// Stand in for the parent's watcher noticing the edited file.
	require.Eventually(t, func() bool {
		parent.Skills.ReplaceDiscovery([]*skills.Skill{edited}, []*skills.Skill{edited}, nil)
		for _, s := range lh.app.Skills.ActiveSkills() {
			if s.Name == "parent-skill" && s.Instructions == "Edited instructions." {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the edit never reached the running thread")
}

// A thread must run the model the parent workspace is actually running,
// not the one its config file happens to hold. The parent's selection can
// be an in-memory override (a session's pinned model, `sennit run
// --model`) that was never persisted, and a thread bootstraps its own
// config from disk.
func TestLocalSpawnerInheritsParentModel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { db.ResetPool() })

	parent := config.SelectedModel{Provider: "openai", Model: "gpt-5.6-sol"}
	spawner := NewLocalSpawner(nil, nil, nil, func() config.SelectedModel { return parent })

	handle, err := spawner.Spawn(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), handle.ID())) })

	lh, ok := handle.(*localHandle)
	require.True(t, ok)
	require.Equal(t, parent, lh.app.Config().Model)
}

// Restarting a thread is a fresh Spawn, so the parent's model is read
// then -- not captured once when the spawner was built. A thread the user
// stops and starts again after switching models must come back on the new
// one.
func TestLocalSpawnerReadsParentModelPerSpawn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Cleanup(func() { db.ResetPool() })

	current := config.SelectedModel{Provider: "openai", Model: "first"}
	spawner := NewLocalSpawner(nil, nil, nil, func() config.SelectedModel { return current })

	first, err := spawner.Spawn(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), first.ID())) })
	require.Equal(t, current, first.(*localHandle).app.Config().Model)

	current = config.SelectedModel{Provider: "anthropic", Model: "second"}
	second, err := spawner.Spawn(context.Background(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), second.ID())) })
	require.Equal(t, current, second.(*localHandle).app.Config().Model)
}
