package thread_test

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestIsolatedTaskSendRestoresRuntimeAndQueuesSpecializedFollowup(t *testing.T) {
	store := thread.NewStoreForTest(t)
	spawner := newFakeSpawner(t)
	manager := thread.NewManager(thread.ManagerOptions{Store: store, Spawner: spawner, RepoRoot: t.TempDir()})
	shutdownManagerOnCleanup(t, manager)
	path := t.TempDir()
	row, err := store.Create(t.Context(), thread.CreateParams{
		Name: "resume", Kind: thread.KindTask, SessionID: "original-session", ParentSessionID: "parent",
		WorktreePath: path, Branch: "thread/resume", BaseBranch: "main", Execution: "frozen specialization",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), row.ID, thread.SetStatusParams{Status: thread.StatusInterrupted})
	require.NoError(t, err)
	started := make(chan string, 2)
	finishFirst := make(chan struct{})
	t.Cleanup(func() { close(finishFirst) })
	factory := func(goal string, wait bool) thread.TaskRunFactory {
		return func(_ context.Context, sessionID string) (func(context.Context) (thread.TaskRunResult, error), func(), error) {
			if sessionID != "original-session" {
				return nil, nil, context.Canceled
			}
			return func(ctx context.Context) (thread.TaskRunResult, error) {
				started <- goal
				if wait {
					select {
					case <-finishFirst:
					case <-ctx.Done():
						return thread.TaskRunResult{}, ctx.Err()
					}
				}
				return thread.TaskRunResult{Text: goal}, nil
			}, nil, nil
		}
	}
	tasks := thread.NewTaskManagerFromManager(manager, nil, nil, func(ctx context.Context, args thread.TaskCreateArgs) (thread.TaskRuntime, error) {
		require.True(t, args.Resume)
		require.Equal(t, path, args.WorktreePath)
		require.Equal(t, "original-session", args.SessionID)
		require.Equal(t, row.ID, args.DelegationID)
		require.Equal(t, "frozen specialization", args.Execution)
		require.Nil(t, args.Factory)
		handle, err := spawner.Spawn(ctx, thread.SpawnRequest{Path: args.WorktreePath})
		return thread.TaskRuntime{
			Handle: handle, Spawner: spawner, Depth: 2, Factory: factory(args.Goal, true),
			Followup: func(_ context.Context, goal string) (thread.TaskRunFactory, error) {
				return factory(goal, false), nil
			},
		}, err
	})
	disposition, err := tasks.Send(t.Context(), row.ID, "resume selected agent")
	require.NoError(t, err)
	require.True(t, disposition.Resumed)
	select {
	case goal := <-started:
		require.Equal(t, "resume selected agent", goal)
	case <-time.After(5 * time.Second):
		t.Fatal("resumed runner did not start")
	}
	disposition, err = tasks.Send(t.Context(), row.ID, "specialized followup")
	require.NoError(t, err)
	require.True(t, disposition.Queued)
	finishFirst <- struct{}{}
	select {
	case goal := <-started:
		require.Equal(t, "specialized followup", goal)
	case <-time.After(5 * time.Second):
		t.Fatal("queued specialized runner did not start")
	}
	require.Eventually(t, func() bool {
		current, err := store.Get(t.Context(), row.ID)
		return err == nil && current.Status == thread.StatusCompleted && current.ResultSummary == "specialized followup"
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, 1, spawner.spawns())
}
