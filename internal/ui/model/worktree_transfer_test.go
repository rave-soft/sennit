package model

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

type worktreeRootWorkspace struct {
	rootTestWorkspace
	target  workspace.Workspace
	enter   int
	err     error
	release func()
}

func (w *worktreeRootWorkspace) EnterWorktree(context.Context, string) (workspace.Workspace, func(), error) {
	w.enter++
	return w.target, w.release, w.err
}

func (w *worktreeRootWorkspace) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	return nil, nil, nil
}

type worktreeTargetWorkspace struct{ rootTestWorkspace }

func (w *worktreeTargetWorkspace) EnterWorktree(context.Context, string) (workspace.Workspace, func(), error) {
	return nil, nil, nil
}

func (w *worktreeTargetWorkspace) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	return nil, nil, nil
}

func (w *worktreeTargetWorkspace) WorktreeState() workspace.WorktreeState {
	return workspace.WorktreeState{Name: "durable", Path: "/tmp/durable", Phase: "stable", Active: true}
}

func TestWorktreeTransferPreservesForegroundUIAndFencesRepeatedRequests(t *testing.T) {
	target := &worktreeTargetWorkspace{}
	rootWS := &worktreeRootWorkspace{target: target}
	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = rootWS
	r.main.com = r.com
	original := r.main
	original.focus = uiFocusMain
	first := r.transferWorktreeCmd("durable", false)
	require.NotNil(t, first)
	require.Nil(t, r.transferWorktreeCmd("durable", false))
	msg := first().(worktreeTransferMsg)
	_, _ = r.handleWorktreeTransfer(msg)
	require.Same(t, original, r.main)
	require.Equal(t, uiFocusMain, r.main.focus)
	require.Equal(t, "durable", r.main.crumbRoot)
	require.Same(t, target, r.com.Workspace)
	require.Equal(t, 1, rootWS.enter)
}

func TestWorktreeTransferFailureRetainsWorkspaceSubscriptionAndForegroundState(t *testing.T) {
	current := &neutralSubscriberWorkspace{}
	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = current
	r.main.com = r.com
	original := r.main
	stop := current.SubscribeWith(func(any) {})
	r.worktree = &worktreeAttachment{stop: stop, generation: 1}
	r.worktreeGen = 1

	rootWS := &worktreeRootWorkspace{err: errors.New("failed")}
	r.com.Workspace = rootWS
	r.main.com = r.com
	cmd := r.transferWorktreeCmd("durable", false)
	msg := cmd().(worktreeTransferMsg)
	_, _ = r.handleWorktreeTransfer(msg)
	require.Same(t, original, r.main)
	require.Same(t, rootWS, r.com.Workspace)
	require.False(t, current.stopped, "failed transfer must not stop the existing subscription")
	require.NotNil(t, r.worktree)
}

func TestWorktreeTransferSuccessfulCommitStopsPreviousSubscriptionAndCallbackIsIdempotent(t *testing.T) {
	previous := &neutralSubscriberWorkspace{}
	target := &worktreeTargetWorkspace{}
	releases := 0
	rootWS := &worktreeRootWorkspace{target: target, release: func() { releases++ }}
	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = rootWS
	r.main.com = r.com
	r.worktree = &worktreeAttachment{stop: previous.SubscribeWith(func(any) {}), generation: 0}
	cmd := r.transferWorktreeCmd("durable", false)
	msg := cmd().(worktreeTransferMsg)
	_, _ = r.handleWorktreeTransfer(msg)
	require.True(t, previous.stopped)
	require.Equal(t, 1, previous.stopCalls)
	require.Same(t, target, r.com.Workspace)
	r.Cleanup()
	r.Cleanup()
	require.Equal(t, 1, releases)
}

func TestWorktreeTransferDropsStaleGeneration(t *testing.T) {
	r := newTestRoot(t, false)
	r.worktreeGen = 2
	called := 0
	_, cmd := r.handleWorktreeTransfer(worktreeTransferMsg{generation: 1, release: func() { called++ }})
	require.NotNil(t, cmd)
	cmd()
	require.Equal(t, 1, called)
	require.Nil(t, r.worktree)
}

// worktreeExitWorkspace is the workspace an entered worktree presents: its
// ExitWorktree hands the session back to root and returns the callback that
// reaps the worktree App.
type worktreeExitWorkspace struct {
	rootTestWorkspace
	root    workspace.Workspace
	release func()
}

func (w *worktreeExitWorkspace) EnterWorktree(context.Context, string) (workspace.Workspace, func(), error) {
	return nil, nil, errors.New("already in a worktree")
}

func (w *worktreeExitWorkspace) ExitWorktree(context.Context) (workspace.Workspace, func(), error) {
	return w.root, w.release, nil
}

func (w *worktreeExitWorkspace) WorktreeState() workspace.WorktreeState {
	return workspace.WorktreeState{Name: "durable", Path: "/tmp/durable", Phase: "stable", Active: true}
}

func TestWorktreeExitSpendsBothReleaseCallbacksExactlyOnce(t *testing.T) {
	enterReleases, exitReleases := 0, 0
	rootWS := &worktreeRootWorkspace{release: func() { enterReleases++ }}
	entered := &worktreeExitWorkspace{root: rootWS, release: func() { exitReleases++ }}
	rootWS.target = entered

	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = rootWS
	r.main.com = r.com

	enter := r.transferWorktreeCmd("durable", false)
	_, _ = r.handleWorktreeTransfer(enter().(worktreeTransferMsg))
	require.Same(t, entered, r.com.Workspace)
	require.Equal(t, 0, enterReleases, "the worktree App must survive while it owns the session")

	exit := r.transferWorktreeCmd("", true)
	_, cmd := r.handleWorktreeTransfer(exit().(worktreeTransferMsg))
	runWorktreeReleases(t, cmd)
	require.Same(t, rootWS, r.com.Workspace)
	require.Empty(t, r.main.crumbRoot)
	require.Equal(t, 1, enterReleases, "exiting must reap the App that no longer owns the session")
	require.Equal(t, 1, exitReleases)

	// Nothing is left holding a spent callback: a later Cleanup (or a
	// re-entry replacing this attachment) must not reap anything twice.
	require.Nil(t, r.worktree.release)
	r.Cleanup()
	require.Equal(t, 1, enterReleases)
	require.Equal(t, 1, exitReleases)
}

// runWorktreeReleases runs the release callbacks handleWorktreeTransfer
// batched, without running the workspace refresh commands alongside them.
func runWorktreeReleases(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	require.NotNil(t, cmd)
	// tea.Batch collapses to the single cmd when there is nothing to
	// refresh, in which case calling it has already run the callbacks.
	if batch, ok := cmd().(tea.BatchMsg); ok {
		require.NotEmpty(t, batch)
		batch[len(batch)-1]()
	}
}
