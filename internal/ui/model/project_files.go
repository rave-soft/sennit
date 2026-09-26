package model

import (
	"context"
	"log/slog"
	"slices"

	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
)

// loadProjectFileCompletions lists project files through the workspace
// (never internal/fsext directly — the walk must run rooted at the
// workspace's own working directory, which may not be this process's cwd
// once Workspace runs against a remote daemon; see CLIENT-SERVER.md,
// "PR 0.6" and workspace.Workspace.ListProjectFiles) for the @-completion
// popup. depth/limit of 0 mean "use the project's own default" - see
// uiprefs.Prefs.CompletionsDepth/Items's doc comment for when that is.
//
// Runs off the Update goroutine, inside the tea.Cmd completions.Completions.Open
// returns - never call this directly from Update/View.
func loadProjectFileCompletions(ctx context.Context, com *common.Common, depth, limit int) []completions.FileCompletionValue {
	files, err := com.Workspace.ListProjectFiles(ctx, depth, limit)
	if err != nil {
		slog.Warn("Failed to list project files for completions", "error", err)
		return nil
	}
	slices.Sort(files)
	result := make([]completions.FileCompletionValue, len(files))
	for i, f := range files {
		result[i] = completions.FileCompletionValue{Path: f}
	}
	return result
}
