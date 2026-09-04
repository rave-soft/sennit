package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

// questionCancelCountingWorkspace counts QuestionCancel calls; every other
// method is the embedded nil workspace.Workspace, which is fine for a
// detach test that never touches anything else.
type questionCancelCountingWorkspace struct {
	rootTestWorkspace
	cancelCalls int
}

func (w *questionCancelCountingWorkspace) QuestionCancel() bool {
	w.cancelCalls++
	return true
}

// TestThreadAttachmentStateRelease_CancelsPendingQuestion is the
// regression test for finding 1's detach half: destroying an attached
// thread's embedded window used to drop any open QuestionForm without
// ever calling question.Service.Cancel, so the question tool that raised
// it (see forwardQuestions) stayed blocked in Ask forever — detaching
// looked instantaneous, but the thread's tool call never returned.
func TestThreadAttachmentStateRelease_CancelsPendingQuestion(t *testing.T) {
	t.Parallel()

	ws := &questionCancelCountingWorkspace{}
	threadUI := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	cmd := state.release()
	require.NotNil(t, cmd)
	require.Zero(t, ws.cancelCalls, "cancelling must not run on the Update goroutine")

	cmd()
	require.Equal(t, 1, ws.cancelCalls, "detaching must cancel any question still pending on the thread")
}

// TestThreadAttachmentStateCleanup_CancelsPendingQuestion covers the other
// teardown path: Root.Cleanup, run once after program.Run() returns.
func TestThreadAttachmentStateCleanup_CancelsPendingQuestion(t *testing.T) {
	t.Parallel()

	ws := &questionCancelCountingWorkspace{}
	threadUI := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	state.cleanup()
	require.Equal(t, 1, ws.cancelCalls)
}
