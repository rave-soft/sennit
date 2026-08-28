package appws

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- FileTracker --

func (w *AppWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	return workspace.PrepareSessionChangesUsing(ctx, sessionID, w.ListSessionHistory, w.UncommittedFiles)
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
