package workspace

import (
	"context"
	"log/slog"
	"time"

	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
)

// -- FileTracker --

func (w *AppWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]SessionFile, error) {
	return prepareSessionChanges(ctx, sessionID, w.ListSessionHistory, w.UncommittedFiles)
}

func prepareSessionChanges(
	ctx context.Context,
	sessionID string,
	listHistory func(context.Context, string) ([]history.File, error),
	uncommittedFiles func(context.Context) ([]git.FileChange, error),
) ([]SessionFile, error) {
	historyFiles, err := listHistory(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	files := AggregateSessionFiles(historyFiles)
	uncommitted, err := uncommittedFiles(ctx)
	if err != nil {
		slog.Warn("Failed to load uncommitted files for session", "session_id", sessionID, "error", err)
		return files, nil
	}
	return MarkUncommittedSessionFiles(files, uncommitted), nil
}

func (w *AppWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return git.UncommittedFiles(ctx, w.store.WorkingDir())
}

func (w *AppWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) {
	w.app.FileTracker.RecordRead(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.app.FileTracker.ListReadFiles(ctx, sessionID)
}

// -- History --

func (w *AppWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return w.app.History.ListBySessionTree(ctx, sessionID)
}
