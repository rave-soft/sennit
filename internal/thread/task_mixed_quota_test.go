package thread_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestMixedTaskRuntimesShareAdmissionLimits(t *testing.T) {
	store := thread.NewTaskFinalizationStoreForTest(t)
	spawner := newFakeSpawner(t)
	manager := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: spawner, RepoRoot: initRepo(t)})
	shutdownManagerOnCleanup(t, manager)
	parent := newTestParentApp(t)
	factory := func(context.Context, string) (func(context.Context) (thread.TaskRunResult, error), func(), error) {
		return func(ctx context.Context) (thread.TaskRunResult, error) {
			<-ctx.Done()
			return thread.TaskRunResult{}, ctx.Err()
		}, nil, nil
	}
	tasks := thread.NewTaskManagerFromManager(manager, NewTestParentAppSpawner(parent), nil, func(ctx context.Context, args thread.TaskCreateArgs) (thread.TaskRuntime, error) {
		handle, err := spawner.Spawn(ctx, thread.SpawnRequest{Path: args.WorktreePath})
		return thread.TaskRuntime{Handle: handle, Spawner: spawner, Factory: factory}, err
	})
	// Read off the caps rather than spelling them out: the point of this
	// test is that both isolation modes draw on one budget, not what the
	// budget currently is. Parents are filled one at a time, so the parent
	// cap is reached first and the workspace cap exactly at the end.
	perParent := thread.MaxActiveTasksPerParentTurnForTest
	perWorkspace := thread.MaxActiveTasksPerWorkspaceForTest
	for index := range perWorkspace {
		isolation := ""
		if index%2 == 1 {
			isolation = "worktree"
		}
		parent := fmt.Sprintf("parent %d", index/perParent)
		_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: fmt.Sprintf("task %d", index), ParentSessionID: parent, Isolation: isolation, Factory: factory})
		require.NoError(t, err)
		if index == perParent-1 {
			for _, mode := range []string{"", "worktree"} {
				_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "parent overflow", ParentSessionID: "parent 0", Isolation: mode, Factory: factory})
				require.ErrorContains(t, err, fmt.Sprintf("limit %d", perParent))
			}
		}
	}
	for _, mode := range []string{"", "worktree"} {
		_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "workspace overflow", ParentSessionID: "another parent", Isolation: mode, Factory: factory})
		require.ErrorContains(t, err, fmt.Sprintf("limit %d", perWorkspace))
	}
	rows, err := tasks.List(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, perWorkspace, "every admitted task is stored, and nothing past the cap was")
}
