package thread_test

import (
	"path/filepath"
	"testing"

	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestPendingIsolatedTaskRecoveryPreservesDepthAndOutbox(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "present"}[exists], func(t *testing.T) {
			store := thread.NewTaskFinalizationStoreForTest(t)
			manager, _, _ := newTestTaskManager(t, store)
			path := t.TempDir()
			if !exists {
				path = filepath.Join(path, "missing")
			}
			row, err := store.Create(t.Context(), thread.CreateParams{
				Name: "pending", Kind: thread.KindTask, ParentSessionID: "parent", SessionID: "reserved-child", Depth: 2,
				WorktreePath: path,
			})
			require.NoError(t, err)
			require.NoError(t, manager.Recover(t.Context()))
			got, err := store.Get(t.Context(), row.ID)
			require.NoError(t, err)
			require.Equal(t, 2, got.CompletionDepth)
			require.True(t, got.CompletionPending)
			if exists {
				require.Equal(t, thread.StatusInterrupted, got.Status)
			} else {
				require.Equal(t, thread.StatusFailed, got.Status)
			}
		})
	}
}

func TestIsolatedTaskRecoveryDetectsMissingWorktree(t *testing.T) {
	store := thread.NewStoreForTest(t)
	manager, _, _ := newTestTaskManager(t, store)
	row, err := store.Create(t.Context(), thread.CreateParams{
		Name: "missing", Kind: thread.KindTask, ParentSessionID: "parent",
		WorktreePath: filepath.Join(t.TempDir(), "missing"),
	})
	require.NoError(t, err)
	require.NoError(t, manager.Recover(t.Context()))
	got, err := store.Get(t.Context(), row.ID)
	require.NoError(t, err)
	require.Equal(t, thread.StatusFailed, got.Status)
	require.Equal(t, "worktree missing on recovery", got.Error)
}

func TestTaskIsolationRejectsUnknownBeforeAdmission(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	_, err := tasks.Create(t.Context(), thread.TaskCreateArgs{Goal: "inspect", ParentSessionID: "parent", Isolation: "container"})
	require.ErrorContains(t, err, "isolation must be empty or worktree")
	rows, err := store.ListAll(t.Context())
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestIsolatedTaskNeverResumesInParent(t *testing.T) {
	store := thread.NewStoreForTest(t)
	_, tasks, _ := newTestTaskManager(t, store)
	row, err := store.Create(t.Context(), thread.CreateParams{Name: "isolated", Kind: thread.KindTask, WorktreePath: "/preserved/worktree", ParentSessionID: "parent"})
	require.NoError(t, err)
	_, err = tasks.Send(t.Context(), row.ID, "continue")
	require.ErrorContains(t, err, "worktree has been preserved")
}
