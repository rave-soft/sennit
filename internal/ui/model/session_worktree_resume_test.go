package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// sessionResumeWorkspace is rootTestWorkspace plus the handful of
// SessionStore/FileServices methods sessionLoadResolver.resolve needs to
// run a real session load end to end, and a settable ResumeWorktree
// (CLIENT-SERVER.md, PR 2.4b) -- see resolve's own comment on why it is
// tried on every resumable load.
type sessionResumeWorkspace struct {
	rootTestWorkspace

	sess session.Session

	resumeWS        workspace.Workspace
	resumeRelease   func()
	resumeErr       error
	resumeCalls     int
	resumeSessionID string
}

func (w *sessionResumeWorkspace) GetSession(context.Context, string) (session.Session, error) {
	return w.sess, nil
}

func (w *sessionResumeWorkspace) AgentIsSessionBusy(string) bool { return false }

func (w *sessionResumeWorkspace) ApplySessionModel(context.Context, string) (bool, error) {
	return false, nil
}

func (w *sessionResumeWorkspace) PrepareSessionChanges(context.Context, string) ([]workspace.SessionFile, error) {
	return nil, nil
}

func (w *sessionResumeWorkspace) FileTrackerListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func (w *sessionResumeWorkspace) ListMessages(context.Context, string) ([]message.Message, error) {
	return nil, nil
}

func (w *sessionResumeWorkspace) ResumeWorktree(_ context.Context, sessionID string) (workspace.Workspace, func(), error) {
	w.resumeCalls++
	w.resumeSessionID = sessionID
	if w.resumeErr != nil {
		return nil, nil, w.resumeErr
	}
	return w.resumeWS, w.resumeRelease, nil
}

// resumeTargetWorkspace is the worktree workspace ResumeWorktree hands
// back: just enough of workspace.Workspace (via rootTestWorkspace) plus
// its own WorktreeState for Root.applyResumedWorktree to read a name
// from.
type resumeTargetWorkspace struct {
	rootTestWorkspace
	state workspace.WorktreeState
}

func (w *resumeTargetWorkspace) WorktreeState() workspace.WorktreeState { return w.state }

// AgentIsSessionBusy: applyLoadSession reads this once the switch to the
// resumed workspace has already happened (Root.applyResumedWorktree runs
// before the loadSessionMsg reaches the owning *UI), so the target
// workspace needs its own answer rather than rootTestWorkspace's nil
// embed.
func (w *resumeTargetWorkspace) AgentIsSessionBusy(string) bool { return false }

// loadSessionAndRoute drives a real beginSessionLoad through r.main and
// feeds the resulting loadSessionMsg back into r.Update, exactly as the
// Bubble Tea runtime would (tea.Cmd off the Update goroutine, its result
// delivered back to Update).
func loadSessionAndRoute(t *testing.T, r *Root, sessionID string) {
	t.Helper()
	cmd := r.main.beginSessionLoad(sessionID)
	require.NotNil(t, cmd)
	msg, ok := cmd().(loadSessionMsg)
	require.True(t, ok, "beginSessionLoad's cmd must produce a loadSessionMsg")
	_, _ = r.Update(msg)
}

// TestSessionLoad_ResumesOrphanedWorktree covers CLIENT-SERVER.md PR
// 2.4b's own scenario at the UI layer: a session load whose
// ResumeWorktree finds a live worktree workspace still owning the
// session switches Root over to it, the same way handleWorktreeTransfer
// does after an explicit EnterWorktree.
func TestSessionLoad_ResumesOrphanedWorktree(t *testing.T) {
	target := &resumeTargetWorkspace{state: workspace.WorktreeState{Name: "feature", Path: "/tmp/feature", Phase: "stable", Active: true}}
	released := 0
	root := &sessionResumeWorkspace{
		sess:          session.Session{ID: "sess-1"},
		resumeWS:      target,
		resumeRelease: func() { released++ },
	}
	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = root
	r.main.com = r.com

	loadSessionAndRoute(t, r, "sess-1")

	require.Equal(t, 1, root.resumeCalls)
	require.Equal(t, "sess-1", root.resumeSessionID)
	require.Same(t, target, r.com.Workspace)
	require.NotNil(t, r.worktree)
	require.Equal(t, "feature", r.worktree.name)
	require.Zero(t, released, "the resumed hold belongs to the new attachment until it is torn down, not spent immediately")

	r.Cleanup()
	require.Equal(t, 1, released, "Cleanup must eventually spend the resumed release exactly once")
}

// TestSessionLoad_NoWorktreeToResumeStaysOnRoot is the ordinary case:
// ResumeWorktree answers workspace.ErrNoWorktreeForSession (no worktree
// at all, or the in-process TUI's own registry, which is always empty --
// see resolve's comment), and Root must stay exactly where it was.
func TestSessionLoad_NoWorktreeToResumeStaysOnRoot(t *testing.T) {
	root := &sessionResumeWorkspace{
		sess:      session.Session{ID: "sess-1"},
		resumeErr: workspace.ErrNoWorktreeForSession,
	}
	r := NewRoot(newTestRoot(t, false).com, "", false, withGOOS("linux"))
	r.com.Workspace = root
	r.main.com = r.com

	loadSessionAndRoute(t, r, "sess-1")

	require.Equal(t, 1, root.resumeCalls)
	require.Same(t, root, r.com.Workspace)
	require.Nil(t, r.worktree)
}
