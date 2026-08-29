package git

import (
	"context"
	"fmt"
)

// abortMerge aborts an in-progress merge in worktree, restoring it to the
// pre-merge state.
//
// It is test-only on purpose. The thread merge flow never aborts: when
// MergeIntoWorktree hits conflicts it leaves the worktree mid-merge and
// reports the paths, and the design is for the agent to resolve them in
// place and merge again — mergeAttempt re-checks ConflictedFiles on entry
// for exactly that. So the only caller is a test tidying up a repository
// it shares with later cases, and an exported AbortMerge in git.go read as
// a step the conflict path had forgotten to take.
func abortMerge(ctx context.Context, worktree string) error {
	if _, err := run(ctx, worktree, "merge", "--abort"); err != nil {
		return fmt.Errorf("git: merge abort: %w", err)
	}
	return nil
}
