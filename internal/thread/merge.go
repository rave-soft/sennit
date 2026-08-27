package thread

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rave-soft/sennit/internal/git"
)

// mergeAttempt folds a thread's branch back into its base branch.
// allowRetry permits one recursive retry from the top of the merge
// (re-merging base into the thread branch) when the base has moved on
// concurrently; the retry itself passes allowRetry=false so a persistently
// moving base ends in merge_blocked rather than looping. resultSummary, if
// non-empty, is the agent's final text to record alongside every status
// transition this attempt makes; pass "" to preserve whatever is already
// stored (see Merge).
func (m *Manager) mergeAttempt(ctx context.Context, threadID string, allowRetry bool, resultSummary string) error {
	st, err := m.store.Get(ctx, threadID)
	if err != nil {
		return err
	}
	if resultSummary == "" {
		resultSummary = st.ResultSummary
	}

	st, err = m.lc.setStatus(ctx, threadID, StatusMerging, "", resultSummary, 0)
	if err != nil {
		return err
	}

	// A prior attempt may have left the worktree mid-merge. If conflicts
	// are still unresolved, report that state rather than re-attempting;
	// the caller resolves them (via Send) and calls Merge again.
	if conflicts, err := git.ConflictedFiles(ctx, st.WorktreePath); err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	} else if len(conflicts) > 0 {
		return m.setConflict(ctx, threadID, resultSummary, conflicts)
	}

	commitMsg := fmt.Sprintf("thread(%s): %s", st.Name, firstLine(st.Goal))
	if _, err := git.CommitAll(ctx, st.WorktreePath, commitMsg); err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	}

	result, err := git.MergeIntoWorktree(ctx, st.WorktreePath, st.BaseBranch)
	if err != nil {
		return m.blockMerge(ctx, threadID, resultSummary, err.Error())
	}
	if !result.Merged {
		return m.setConflict(ctx, threadID, resultSummary, result.Conflicts)
	}

	ffErr := git.FastForward(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	switch {
	case ffErr == nil:
		return m.finishMerge(ctx, threadID, resultSummary)

	case errors.Is(ffErr, git.ErrBranchCheckedOut):
		dirty, err := git.IsDirty(ctx, m.repoRoot)
		if err != nil {
			return m.blockMerge(ctx, threadID, resultSummary, err.Error())
		}
		if dirty {
			return m.blockMerge(ctx, threadID, resultSummary, "base branch is checked out and the main worktree is dirty: "+ffErr.Error())
		}
		if err := git.MergeFFOnly(ctx, m.repoRoot, st.Branch); err != nil {
			return m.blockMerge(ctx, threadID, resultSummary, err.Error())
		}
		return m.finishMerge(ctx, threadID, resultSummary)

	case errors.Is(ffErr, git.ErrNonFastForward):
		if allowRetry {
			return m.mergeAttempt(ctx, threadID, false, resultSummary)
		}
		return m.blockMerge(ctx, threadID, resultSummary, "base branch moved concurrently: "+ffErr.Error())

	default:
		return m.blockMerge(ctx, threadID, resultSummary, ffErr.Error())
	}
}

func (m *Manager) finishMerge(ctx context.Context, threadID, resultSummary string) error {
	st, err := m.lc.setStatus(ctx, threadID, StatusMerged, "", resultSummary, time.Now().Unix())
	if err != nil {
		return err
	}
	if c := m.lc.existingControl(threadID); c != nil {
		c.mu.Lock()
		rt := c.runtime
		c.runtime = nil
		c.mu.Unlock()
		if rt != nil {
			// Cancel this thread's own session, not the whole App's
			// coordinator. Merge is guarded to threads only (see Merge's
			// Kind check), whose App is their own, so this is equivalent
			// to CancelAll today — but CancelAll on a kind that shares its
			// App with something else (a task's parent) would cancel
			// unrelated work, so this stays correct if that guard is ever
			// loosened.
			if err := releaseRuntime(ctx, rt, st.SessionID, true); err != nil {
				slog.Error("Failed to release merged workspace", "component", "thread", "thread", threadID, "error", err)
			}
		}
	}
	m.lc.publish(EventMerged, st)
	return nil
}

// discardMerged clears away a thread whose branch has landed: its work is
// in the base branch, so the worktree and branch are duplicates and the
// row is a row about nothing. Both merge paths call it — the automatic one
// and Manager.Merge — and it leaves a note in the parent session's history
// so the thread's disappearance from the panel is something the user can
// read about rather than something they have to notice.
//
// Callers must hold the thread's opMu; both do. Every step is best-effort
// and only reports through the log: the merge itself has already
// succeeded and been delivered by the time this runs, so nothing here may
// turn a landed merge into a failure. If the worktree cannot be removed,
// the row deliberately stays — a thread still visible as merged is a far
// better outcome than a row-less directory nothing knows how to clean up.
//
// The thread's own session and its messages are untouched. They are the
// record of how the work was done, and dropping them would make the
// history entry point at nothing.
func (m *Manager) discardMerged(ctx context.Context, threadID string) {
	st, err := m.store.Get(ctx, threadID)
	if err != nil {
		slog.Error("Failed to re-fetch merged thread for discard", "component", "thread", "thread", threadID, "error", err)
		return
	}
	// Only a merge that landed. A conflict or a block still owns its
	// worktree — that is exactly where the user resolves it.
	if st.Kind != KindThread || st.Status != StatusMerged {
		return
	}

	if err := git.WorktreeRemove(ctx, m.repoRoot, st.WorktreePath, true); err != nil {
		slog.Error("Failed to remove merged worktree", "component", "thread", "thread", threadID, "error", err)
		return
	}
	branchKept := m.keepMergedBranch(ctx, st)
	if err := m.store.Delete(ctx, st.ID); err != nil {
		slog.Error("Failed to delete merged thread record", "component", "thread", "thread", threadID, "error", err)
		return
	}
	m.lc.publish(EventRemoved, st)
	m.recordDiscardNotice(ctx, st, branchKept)
}

// keepMergedBranch deletes the branch of a merged thread that is being
// discarded, and reports whether the branch is still there afterwards — the
// one detail the note to the user turns on.
//
// It verifies the branch really is contained in the base before deleting
// it, and only then forces. "git branch -d" is not that check: it asks
// whether the branch is merged into HEAD, so it refuses a properly merged
// branch whenever the repo has something else checked out — which for a
// thread's base branch is the normal case, not the exception. Ask about the
// two branches that actually matter, then act on the answer.
func (m *Manager) keepMergedBranch(ctx context.Context, st Thread) bool {
	if exists, err := git.BranchExists(ctx, m.repoRoot, st.Branch); err == nil && !exists {
		// Already deleted, by hand or by an earlier run. That is the state
		// this wants, so there is nothing to verify and nothing to report.
		return false
	}
	merged, err := git.IsAncestor(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	switch {
	case err != nil:
		slog.Error("Failed to verify merged branch before delete", "component", "thread", "thread", st.ID, "branch", st.Branch, "error", err)
	case !merged:
		slog.Error("Refusing to delete a branch not contained in its base", "component", "thread", "thread", st.ID, "branch", st.Branch, "base", st.BaseBranch)
	default:
		if err := git.DeleteBranch(ctx, m.repoRoot, st.Branch, true); err != nil {
			slog.Error("Failed to delete merged branch", "component", "thread", "thread", st.ID, "branch", st.Branch, "error", err)
		} else {
			return false
		}
	}
	return true
}

// recordDiscardNotice writes the merged-and-removed note into the parent
// session's history.
//
// It is a system-role message on purpose: that role is dropped when the
// prompt is built (see message.Message conversion), so this is a record
// for the user only. The model has already been told the thread finished,
// through the completion inbox — telling it a second time, in a different
// voice, would be the kind of duplicate that makes an agent repeat itself.
func (m *Manager) recordDiscardNotice(ctx context.Context, st Thread, branchKept bool) {
	a, sessionID, ok := m.resolveDeliveryTarget(ctx, nil, st)
	if !ok || a == nil || a.Messages() == nil {
		return
	}
	text := fmt.Sprintf("Thread %q merged into %s and removed.", st.Name, st.BaseBranch)
	if branchKept {
		text = fmt.Sprintf("Thread %q merged into %s and removed; branch %s kept (git declined to delete it).",
			st.Name, st.BaseBranch, st.Branch)
	}
	if err := a.Messages().Create(ctx, sessionID, RoleSystem, []ContentPart{TextContent{Text: text}}); err != nil {
		slog.Error("Failed to record thread removal in history", "component", "thread", "thread", st.ID, "error", err)
	}
}

func (m *Manager) setConflict(ctx context.Context, threadID, resultSummary string, conflicts []string) error {
	_, err := m.lc.setStatus(ctx, threadID, StatusConflict, "merge conflicts: "+strings.Join(conflicts, ", "), resultSummary, 0)
	return err
}

func (m *Manager) blockMerge(ctx context.Context, threadID, resultSummary, reason string) error {
	_, err := m.lc.setStatus(ctx, threadID, StatusMergeBlocked, reason, resultSummary, 0)
	return err
}
