package thread

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/rave-soft/sennit/internal/git"
)

const (
	cleanupDirty      = "cleanup retained: uncommitted changes"
	cleanupCommits    = "cleanup retained: unique commits"
	cleanupUnverified = "cleanup retained: safety could not be verified"
)

func (m *Manager) onCompletedCleanup(ctx context.Context, c *threadControl, st Thread, resultText string) bool {
	if st.Kind != KindThread {
		return false
	}
	c.mu.Lock()
	runtimeLive := c.runtime != nil
	c.mu.Unlock()
	if runtimeLive {
		m.finishRetainedCleanup(ctx, st, resultText, cleanupUnverified)
		return true
	}
	m.discardCompleted(ctx, st, resultText)
	return true
}

func (m *Manager) discardCompleted(ctx context.Context, st Thread, resultText string) {
	reason := m.cleanupSafety(ctx, st)
	if reason != "" {
		m.finishRetainedCleanup(ctx, st, resultText, reason)
		return
	}
	branchTip, err := git.ResolveCommit(ctx, m.repoRoot, st.Branch)
	if err != nil {
		m.finishRetainedCleanup(ctx, st, resultText, cleanupUnverified)
		return
	}

	prepared, err := m.store.SetStatus(ctx, st.ID, SetStatusParams{
		Status:        StatusCompleted,
		Error:         cleanupUnverified,
		ResultSummary: resultText,
		CompletedAt:   time.Now().Unix(),
	})
	if err != nil {
		m.finishRetainedCleanup(ctx, st, resultText, cleanupUnverified)
		return
	}

	if err := m.worktreeRemove(ctx, m.repoRoot, st.WorktreePath, false); err != nil {
		m.reconcileCleanupFailure(ctx, prepared, resultText, branchTip)
		return
	}
	if err := m.deleteBranch(ctx, m.repoRoot, st.Branch, true); err != nil {
		m.reconcileCleanupFailure(ctx, prepared, resultText, branchTip)
		return
	}
	if err := m.store.Delete(ctx, st.ID); err != nil {
		m.reconcileCleanupFailure(ctx, prepared, resultText, branchTip)
		return
	}

	final := prepared
	final.Error = ""
	m.lc.deliverStoredCompletion(ctx, nil, final, 0)
	m.lc.publish(EventRemoved, final)
	m.recordDiscardNotice(ctx, final, "")
}

func (m *Manager) reconcileCleanupFailure(ctx context.Context, st Thread, resultText, _ string) {
	final, err := m.store.Get(context.WithoutCancel(ctx), st.ID)
	if err != nil {
		branchExists, branchErr := git.BranchExists(context.WithoutCancel(ctx), m.repoRoot, st.Branch)
		if branchErr == nil && !branchExists && !pathExists(st.WorktreePath) {
			st.Status = StatusCompleted
			st.Error = ""
			st.ResultSummary = resultText
			m.lc.deliverStoredCompletion(context.WithoutCancel(ctx), nil, st, 0)
			m.lc.publish(EventRemoved, st)
			m.recordDiscardNotice(context.WithoutCancel(ctx), st, "")
		}
		return
	}

	branchExists, branchErr := git.BranchExists(context.WithoutCancel(ctx), m.repoRoot, st.Branch)
	if branchErr != nil {
		branchExists = false
	}
	worktreeExists := pathExists(st.WorktreePath)
	m.lc.publish(EventStatusChanged, final)
	m.lc.deliverStoredCompletion(context.WithoutCancel(ctx), nil, final, 0)
	m.recordDiscardNotice(context.WithoutCancel(ctx), final, cleanupResources(final, branchExists, worktreeExists))
}

func cleanupResources(st Thread, branchExists, worktreeExists bool) string {
	resources := make([]string, 0, 3)
	if worktreeExists {
		resources = append(resources, "worktree")
	}
	if branchExists {
		resources = append(resources, "branch")
	}
	if st.ID != "" {
		resources = append(resources, "record")
	}
	if len(resources) == 0 {
		return cleanupUnverified + "; no managed resources could be confirmed"
	}
	return cleanupUnverified + "; available: " + strings.Join(resources, ", ")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (m *Manager) cleanupSafety(ctx context.Context, st Thread) string {
	if st.WorktreePath == "" || st.Branch == "" || st.BaseBranch == "" {
		return cleanupUnverified
	}
	dirty, err := git.IsDirty(ctx, st.WorktreePath)
	if err != nil {
		return cleanupUnverified
	}
	if dirty {
		return cleanupDirty
	}
	contained, err := git.IsAncestor(ctx, m.repoRoot, st.Branch, st.BaseBranch)
	if err != nil {
		return cleanupUnverified
	}
	if !contained {
		return cleanupCommits
	}
	return ""
}

func (m *Manager) finishRetainedCleanup(ctx context.Context, st Thread, resultText, reason string) {
	final, err := m.lc.setStatus(ctx, st.ID, StatusCompleted, reason, resultText, time.Now().Unix())
	if err != nil {
		slog.Error("Failed to record retained thread cleanup outcome", "component", "thread", "thread", st.ID, "error", err)
		return
	}
	branchExists, branchErr := git.BranchExists(context.WithoutCancel(ctx), m.repoRoot, st.Branch)
	if branchErr != nil {
		branchExists = false
	}
	worktreeExists := pathExists(st.WorktreePath)
	m.lc.deliverStoredCompletion(ctx, nil, final, 0)
	m.recordDiscardNotice(ctx, final, cleanupResources(final, branchExists, worktreeExists))
}

func (m *Manager) recordDiscardNotice(ctx context.Context, st Thread, outcome string) {
	a, sessionID, ok := m.resolveDeliveryTarget(ctx, nil, st)
	if !ok || a == nil || a.Messages() == nil {
		return
	}
	text := fmt.Sprintf("Thread %q completed; its clean worktree and branch were removed.", st.Name)
	if outcome != "" {
		text = fmt.Sprintf("Thread %q completed; %s.", st.Name, outcome)
	}
	if err := a.Messages().Create(ctx, sessionID, RoleSystem, []ContentPart{TextContent{Text: text}}); err != nil {
		slog.Error("Failed to record thread cleanup in history", "component", "thread", "thread", st.ID, "error", err)
	}
}
