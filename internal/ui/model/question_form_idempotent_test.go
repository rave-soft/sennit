package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// TestOpenBatchFormDialog_SameBatchIsIdempotent is the regression test for
// the second half of finding 1: while drilled into a thread, the same
// question batch can reach this UI twice (once through the thread's own
// event pump, once through the parent's relayed stream — see
// lifecycle.forwardQuestions). openBatchFormDialog used to throw away
// whatever form was already open and rebuild a fresh one for every
// delivery, discarding any answers the person had already entered — the
// same trap openPermissionsDialog's doc explains for permissions.
func TestOpenBatchFormDialog_SameBatchIsIdempotent(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	batch := question.Request{
		ID: "batch-1",
		Questions: []question.Question{{
			ID:   "q1",
			Type: question.TypeYesNo,
			Text: "Ready?",
		}},
	}
	u.openBatchFormDialog(batch)
	first, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok, "opening a batch must install a QuestionForm")

	// A second delivery of the identical batch must leave the open form
	// alone rather than replacing it.
	u.openBatchFormDialog(batch)
	second, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)
	require.Same(t, first, second, "a duplicate delivery of the same batch must not replace the open form")

	// A genuinely new batch still replaces it.
	u.openBatchFormDialog(question.Request{
		ID: "batch-2",
		Questions: []question.Question{{
			ID:   "q2",
			Type: question.TypeYesNo,
			Text: "Still ready?",
		}},
	})
	third, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)
	require.NotSame(t, first, third, "a new batch id must still replace the open form")
	require.Equal(t, "batch-2", third.BatchID)
}
