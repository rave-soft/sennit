package workspace

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"

	"github.com/rave-soft/sennit/internal/diff"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
)

// SessionFile is the aggregated history and diff information for one file.
type SessionFile struct {
	FirstVersion  history.File
	LatestVersion history.File
	Additions     int
	Deletions     int
	Uncommitted   bool
}

// SessionChangePreparer prepares the files changed by a session.
type SessionChangePreparer interface {
	PrepareSessionChanges(ctx context.Context, sessionID string) ([]SessionFile, error)
}

func AggregateSessionFiles(files []history.File) []SessionFile {
	byPath := make(map[string][]history.File)
	for _, file := range files {
		byPath[file.Path] = append(byPath[file.Path], file)
	}
	result := make([]SessionFile, 0, len(byPath))
	for _, versions := range byPath {
		first, last := versions[0], versions[0]
		for _, file := range versions[1:] {
			if file.Version < first.Version {
				first = file
			}
			if file.Version > last.Version {
				last = file
			}
		}
		_, additions, deletions := diff.GenerateDiff(first.Content, last.Content, first.Path)
		result = append(result, SessionFile{FirstVersion: first, LatestVersion: last, Additions: additions, Deletions: deletions})
	}
	slices.SortFunc(result, func(a, b SessionFile) int {
		if a.LatestVersion.UpdatedAt > b.LatestVersion.UpdatedAt {
			return -1
		}
		if a.LatestVersion.UpdatedAt < b.LatestVersion.UpdatedAt {
			return 1
		}
		return 0
	})
	return result
}

// PrepareSessionChangesUsing aggregates sessionID's file history into
// SessionFile entries and marks the ones still uncommitted, given the
// caller's own ListSessionHistory/UncommittedFiles implementations. It is
// exported (rather than a private helper local to one implementation)
// because both AppWorkspace.PrepareSessionChanges (internal/workspace/appws)
// and readOnlyWorkspace.PrepareSessionChanges (in this package) need it,
// scoped to their own working directory/history source.
func PrepareSessionChangesUsing(
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

func MarkUncommittedSessionFiles(sessionFiles []SessionFile, files []git.FileChange) []SessionFile {
	paths := make(map[string]struct{}, len(files))
	for _, file := range files {
		paths[filepath.Clean(file.Path)] = struct{}{}
	}
	result := make([]SessionFile, len(sessionFiles))
	for i, file := range sessionFiles {
		if _, ok := paths[filepath.Clean(file.FirstVersion.Path)]; ok {
			file.Uncommitted = true
		}
		result[i] = file
	}
	return result
}
