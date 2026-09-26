package appws

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- FileTracker --

func (w *AppWorkspace) PrepareSessionChanges(ctx context.Context, sessionID string) ([]workspace.SessionFile, error) {
	return workspace.PrepareSessionChangesUsing(ctx, sessionID, w.ListSessionHistory, func(ctx context.Context, paths []string) ([]string, error) {
		return git.UncommittedPaths(ctx, w.store.WorkingDir(), paths)
	})
}

func (w *AppWorkspace) UncommittedFiles(ctx context.Context) ([]git.FileChange, error) {
	return git.UncommittedFiles(ctx, w.store.WorkingDir())
}

func (w *AppWorkspace) FileTrackerRecordRead(ctx context.Context, sessionID, path string) error {
	return w.app.FileTracker.RecordRead(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerLastReadTime(ctx context.Context, sessionID, path string) (time.Time, error) {
	return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
}

func (w *AppWorkspace) FileTrackerListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return w.app.FileTracker.ListReadFiles(ctx, sessionID)
}

// -- History --

func (w *AppWorkspace) ListSessionHistory(ctx context.Context, sessionID string) ([]history.File, error) {
	return w.app.History.ListBySessionTree(ctx, sessionID)
}

// -- Project files --

// ListProjectFiles implements workspace.Workspace, replacing the direct
// fsext.ListDirectory(".") call the @-mention completions popup used to
// make from internal/ui/completions - see CLIENT-SERVER.md, "PR 0.6". The
// walk itself is unchanged (same ignore behavior, same depth/limit
// semantics); only the root and the "0/0 means unset" default resolution
// are new.
func (w *AppWorkspace) ListProjectFiles(ctx context.Context, depth, limit int) ([]string, error) {
	if depth == 0 && limit == 0 {
		depth, limit = w.store.Config().CompletionsLimits()
	}

	root := w.store.WorkingDir()
	files, _, err := fsext.ListDirectory(root, nil, depth, limit)
	if err != nil {
		return nil, err
	}

	// fsext.ListDirectory returns paths rooted at the argument it was
	// given (root, an absolute path here) rather than relative ones; the
	// completions popup wants the same working-directory-relative form
	// it always has (and directories keep their trailing separator - see
	// ListDirectory's own doc comment). filepath.ToSlash on both sides
	// keeps the prefix trim correct even where root uses "\" (Windows)
	// but fastwalk's ToSlash normalization already turned the entries
	// themselves into "/"-separated paths.
	rootSlash := filepath.ToSlash(root)
	out := make([]string, 0, len(files))
	for _, f := range files {
		rel := strings.TrimPrefix(f, rootSlash)
		rel = strings.TrimPrefix(rel, "/")
		out = append(out, rel)
	}
	return out, nil
}

// AttachProjectFile implements workspace.Workspace, replacing the file
// resolution/read internal/ui/model/editor_input.go used to do directly
// against the process's own cwd and disk - see CLIENT-SERVER.md, "PR 0.6".
func (w *AppWorkspace) AttachProjectFile(ctx context.Context, sessionID, path string) (message.Attachment, bool, error) {
	// Wrapped in a closure rather than passed as the bound method value
	// w.app.FileTracker.LastReadTime: with sessionID == "" (no session
	// yet) AttachProjectFileUsing never calls this at all, and a nil
	// FileTracker (only ever true in a test that skips that branch) must
	// not panic just from taking the method value.
	lastReadTime := func(ctx context.Context, sessionID, path string) (time.Time, error) {
		return w.app.FileTracker.LastReadTime(ctx, sessionID, path)
	}
	return workspace.AttachProjectFileUsing(ctx, w.store.WorkingDir(), sessionID, path, lastReadTime)
}
