package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// questionCancelCountingWorkspace counts QuestionCancel calls and records
// the batch ID each was made with; every other method is the embedded nil
// workspace.Workspace, which is fine for a detach test that never touches
// anything else.
type questionCancelCountingWorkspace struct {
	rootTestWorkspace
	cancelCalls       int
	lastBatchID       string
	sawEmptyBatch     bool
	cancelledBatchIDs []string
}

func (w *questionCancelCountingWorkspace) QuestionCancel(batchID string) (bool, error) {
	w.cancelCalls++
	w.lastBatchID = batchID
	w.cancelledBatchIDs = append(w.cancelledBatchIDs, batchID)
	if batchID == "" {
		w.sawEmptyBatch = true
	}
	return true, nil
}

// openQuestionForm gives threadUI a displayed QuestionForm for batchID, the
// way openBatchFormDialog would for a question raised inside the thread —
// the state cancelThreadQuestion now reads to target its Cancel call. It
// also tracks batchID in pendingInlineBatches, matching what
// openBatchFormDialog itself does, since that set (not just what is
// currently displayed) is what cancelThreadQuestion sweeps.
func openQuestionForm(threadUI *UI, batchID string) {
	threadUI.activeInline = dialog.NewQuestionForm(threadUI.com.Styles, question.Request{
		ID: batchID,
		Questions: []question.Question{{
			ID:   "q1",
			Type: question.TypeYesNo,
			Text: "Proceed?",
		}},
	})
	threadUI.trackInlineBatch(batchID)
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
	openQuestionForm(threadUI, "batch-1")

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	cmd := state.release()
	require.NotNil(t, cmd)
	require.Zero(t, ws.cancelCalls, "cancelling must not run on the Update goroutine")

	cmd()
	require.Equal(t, 1, ws.cancelCalls, "detaching must cancel any question still pending on the thread")
	require.Equal(t, "batch-1", ws.lastBatchID, "the cancel must target the batch the thread's own form was showing")
}

// TestThreadAttachmentStateCleanup_CancelsPendingQuestion covers the other
// teardown path: Root.Cleanup, run once after program.Run() returns.
func TestThreadAttachmentStateCleanup_CancelsPendingQuestion(t *testing.T) {
	t.Parallel()

	ws := &questionCancelCountingWorkspace{}
	threadUI := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())
	openQuestionForm(threadUI, "batch-1")

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	state.cleanup()
	require.Equal(t, 1, ws.cancelCalls)
	require.Equal(t, "batch-1", ws.lastBatchID)
}

// TestThreadAttachmentStateRelease_LeavesParentQuestionAloneWhenThreadHasNone
// is defect 3's regression test: with nothing displayed on the thread's own
// embedded UI, detaching used to still call QuestionCancel with no batch
// ID, which — through attachedThreadWorkspace's own-then-parent fallback —
// could cancel whatever the PARENT workspace happened to have pending
// instead, a question the detach has nothing to do with. Nothing displayed
// here means nothing to cancel: cancelThreadQuestion must not call
// QuestionCancel at all.
func TestThreadAttachmentStateRelease_LeavesParentQuestionAloneWhenThreadHasNone(t *testing.T) {
	t.Parallel()

	ws := &questionCancelCountingWorkspace{}
	threadUI := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())
	// No openQuestionForm call: the thread's own embedded UI has nothing
	// displayed, matching "parent has a pending question, the thread
	// itself has none."

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	cmd := state.release()
	require.NotNil(t, cmd)
	cmd()

	require.Zero(t, ws.cancelCalls, "nothing pending on the thread's own screen means nothing to cancel")
	require.False(t, ws.sawEmptyBatch, "must never call QuestionCancel with an unmatched or empty batch ID")
}

// questionRequest builds a minimal valid single-question batch request for
// the given id, the way openBatchFormDialog expects to receive one off the
// event pump.
func questionRequest(batchID string) question.Request {
	return question.Request{
		ID: batchID,
		Questions: []question.Question{{
			ID:          "q1",
			Type:        question.TypeYesNo,
			Text:        "Proceed?",
			Description: "Confirm before continuing.",
		}},
	}
}

// TestThreadAttachmentStateRelease_CancelsReplacedBatchToo is the mirror
// case a round-1 review found in cancelThreadQuestion: openBatchFormDialog
// (dialogs.go) silently drops a displayed form when a different batch
// arrives, without ever cancelling the one it replaced (see the comment
// there). Batch A is shown, then batch B arrives and replaces it on
// screen; the person detaches before answering either. Both must be
// cancelled — A because it was dropped without resolution, B because it
// was still on screen, unresolved, at detach.
func TestThreadAttachmentStateRelease_CancelsReplacedBatchToo(t *testing.T) {
	t.Parallel()

	ws := &questionCancelCountingWorkspace{}
	threadUI := New(common.DefaultCommon(context.Background(), ws), "", false, WithEmbedded())

	threadUI.openBatchFormDialog(questionRequest("batch-a"))
	threadUI.openBatchFormDialog(questionRequest("batch-b"))

	qf, ok := threadUI.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)
	require.Equal(t, "batch-b", qf.BatchID, "the newer batch replaces the older one on screen")

	state := threadAttachmentState{
		thread: &threadAttachment{threadID: "t1", ui: threadUI},
	}

	cmd := state.release()
	require.NotNil(t, cmd)
	cmd()

	require.Equal(t, 2, ws.cancelCalls, "both the replaced batch and the one left on screen must be cancelled")
	require.ElementsMatch(t, []string{"batch-a", "batch-b"}, ws.cancelledBatchIDs)
}
