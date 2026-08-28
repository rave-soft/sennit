package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/stretchr/testify/require"
)

func TestAggregateSessionFiles(t *testing.T) {
	t.Parallel()

	files := AggregateSessionFiles([]history.File{
		{Path: "b.go", Version: 2, Content: "one\ntwo\n", UpdatedAt: 20},
		{Path: "a.go", Version: 3, Content: "old\nnew\n", UpdatedAt: 30},
		{Path: "b.go", Version: 1, Content: "one\n", UpdatedAt: 10},
		{Path: "a.go", Version: 1, Content: "old\n", UpdatedAt: 5},
	})

	require.Len(t, files, 2)
	require.Equal(t, "a.go", files[0].FirstVersion.Path)
	require.Equal(t, int64(1), files[0].FirstVersion.Version)
	require.Equal(t, int64(3), files[0].LatestVersion.Version)
	require.Equal(t, 1, files[0].Additions)
	require.Equal(t, 0, files[0].Deletions)
	require.Equal(t, "b.go", files[1].FirstVersion.Path)
	require.Equal(t, int64(1), files[1].FirstVersion.Version)
	require.Equal(t, int64(2), files[1].LatestVersion.Version)
	require.Equal(t, 1, files[1].Additions)
	require.Equal(t, 0, files[1].Deletions)
}

func TestMarkUncommittedSessionFilesNormalizesPaths(t *testing.T) {
	t.Parallel()

	files := MarkUncommittedSessionFiles([]SessionFile{
		{FirstVersion: history.File{Path: "dir/../changed.go"}},
		{FirstVersion: history.File{Path: "committed.go"}},
	}, []git.FileChange{{Path: "changed.go"}})

	require.Equal(t, []SessionFile{{FirstVersion: history.File{Path: "dir/../changed.go"}, Uncommitted: true}}, files)
}

func TestPrepareSessionChangesDegradesWhenGitFails(t *testing.T) {
	t.Parallel()

	historyFiles := []history.File{{Path: "main.go", Version: 1, Content: "before\n"}, {Path: "main.go", Version: 2, Content: "after\n"}}
	files, err := PrepareSessionChangesUsing(t.Context(), "session", func(context.Context, string) ([]history.File, error) {
		return historyFiles, nil
	}, func(context.Context) ([]git.FileChange, error) {
		return nil, errors.New("git unavailable")
	})

	require.NoError(t, err)
	require.Len(t, files, 1)
	require.False(t, files[0].Uncommitted)
	require.Equal(t, 1, files[0].Additions)
	require.Equal(t, 1, files[0].Deletions)
}

func TestPrepareSessionChangesPropagatesHistoryError(t *testing.T) {
	t.Parallel()

	expected := errors.New("history unavailable")
	_, err := PrepareSessionChangesUsing(t.Context(), "session", func(context.Context, string) ([]history.File, error) {
		return nil, expected
	}, func(context.Context) ([]git.FileChange, error) {
		t.Fatal("git must not be called after a history error")
		return nil, nil
	})

	require.ErrorIs(t, err, expected)
}
