package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// These tests cover the off-goroutine model access bug fixed across several
// tea.Cmd-returning methods: a returned closure ran on the cmd goroutine but
// dereferenced m.com (or m.com.Workspace) directly instead of a local
// snapshotted before the closure was returned, racing the Update goroutine
// that mutates that state. The fix is the same shape as dispatchBusyRefresh
// (see workspace_cache.go): hoist `ws := m.com.Workspace` (and `ctx :=
// m.com.Context()` where needed) above the closure.
//
// Like TestPasteImageFromClipboardCmd_DoesNotReadModelOffGoroutine, these
// only have teeth under -race: without the detector an unsynchronized
// read/write pair usually "just works", so they are skipped rather than
// giving false confidence.

func TestAttachSkillCmd_DoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	cmd := attachSkill(u.com, "skill-1", "Skill One")
	require.NotNil(t, cmd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd()
	}()

	// Concurrently swap the workspace the pre-fix closure used to read
	// directly while the cmd runs on its own goroutine.
	u.com.Workspace = &cmdDrivingWorkspace{agentReady: true}

	<-done
}

func TestStartLSPsCmd_DoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	cmd := startLSPs(u.com, []string{"main.go"})
	require.NotNil(t, cmd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd()
	}()

	u.com.Workspace = &cmdDrivingWorkspace{agentReady: true}

	<-done
}

func TestLoadMCPromptsCmd_DoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	cmd := loadMCPromptsCmd(u.com, u)
	require.NotNil(t, cmd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd()
	}()

	u.com.Workspace = &cmdDrivingWorkspace{agentReady: true}

	<-done
}

func TestHandleStateChangedCmd_DoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)
	warmCmdDrivenCaches(u)

	cmd := u.handleStateChanged()
	require.NotNil(t, cmd)

	// handleStateChanged sequences its own closure with agentModelChangedCmd
	// (via updateAgentModelCmd); unwrap to the first, the one this test
	// targets, and run just that — not through Update, which would also
	// dispatch unrelated commands (e.g. dispatchBusyRefresh) that
	// legitimately read m.com on this same goroutine and would otherwise
	// race against this test's own concurrent mutation below.
	cmds, ok := isCommandSliceWrapper(cmd())
	require.True(t, ok, "expected handleStateChanged to sequence commands")
	require.NotEmpty(t, cmds)
	leaf := cmds[0]

	done := make(chan struct{})
	go func() {
		defer close(done)
		leaf()
	}()

	u.com.Workspace = &cmdDrivingWorkspace{agentReady: true}

	<-done
}

// TestApplySessionDialogAction_SummarizeDoesNotReadModelOffGoroutine covers
// dialog_actions.go's ActionSummarize branch, which used to call
// m.com.Workspace.AgentSummarize and m.com.Context() from inside the
// returned closure.
func TestApplySessionDialogAction_SummarizeDoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	cmd, handled := u.applySessionDialogAction(dialog.ActionSummarize{SessionID: "s1"})
	require.True(t, handled)
	require.NotNil(t, cmd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd()
	}()

	u.com.Workspace = &cmdDrivingWorkspace{agentReady: true}

	<-done
}
