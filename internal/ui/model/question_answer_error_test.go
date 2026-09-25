package model

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func testQuestionBatch(id string) question.Request {
	return question.Request{
		ID: id,
		Questions: []question.Question{{
			ID:   "q1",
			Type: question.TypeYesNo,
			Text: "Ready?",
		}},
	}
}

// TestQuestionAnswer_ErrorReportedAndFormReopened pins the fix for a
// swallowed error: previously ws.QuestionAnswer's result was discarded
// entirely, so a call that could not be carried out at all (as opposed to
// simply losing the race to resolve an already-decided batch) vanished
// with no error shown and no way to retry. The form is already torn down
// synchronously before the Cmd runs (see openBatchFormDialog's doc
// comment), so "keep it open" isn't available the way it is for the
// permission dialog; the next best thing — reporting the failure and
// reopening a fresh copy of the form — is what this pins.
func TestQuestionAnswer_ErrorReportedAndFormReopened(t *testing.T) {
	t.Parallel()

	answerErr := errors.New("workspace unreachable")
	ws := &cmdDrivingWorkspace{agentReady: true, questionAnswerErr: answerErr}
	u := newCmdDrivenUI(ws)

	batch := testQuestionBatch("batch-answer-err")
	u.openBatchFormDialog(batch)
	form, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok, "opening a batch must install a QuestionForm")

	yes := true
	cmd := form.OnAnswer([]question.Answer{{QuestionID: "q1", Yes: &yes}})
	require.NotNil(t, cmd)
	msg := cmd()

	// The form is gone by the time the result lands, matching the real
	// flow (keypress.go clears activeInline before running this Cmd).
	u.activeInline = nil

	cmds, _ := u.updatePrompts(msg, nil)

	var reported util.InfoMsg
	for _, c := range cmds {
		if c == nil {
			continue
		}
		if info, ok := c().(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			reported = info
			break
		}
	}
	require.Equal(t, util.InfoTypeError, reported.Type, "the answer failure must be reported")
	require.Contains(t, reported.Msg, answerErr.Error())

	reopened, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok, "the form must be reopened so the answer can be retried")
	require.Equal(t, batch.ID, reopened.BatchID)
}

// TestQuestionCancel_ErrorReported is QuestionAnswer's counterpart for
// OnCancel.
func TestQuestionCancel_ErrorReported(t *testing.T) {
	t.Parallel()

	cancelErr := errors.New("workspace unreachable")
	ws := &cmdDrivingWorkspace{agentReady: true, questionCancelErr: cancelErr}
	u := newCmdDrivenUI(ws)

	batch := testQuestionBatch("batch-cancel-err")
	u.openBatchFormDialog(batch)
	form, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)

	cmd := form.OnCancel()
	require.NotNil(t, cmd)
	msg := cmd()
	u.activeInline = nil

	cmds, _ := u.updatePrompts(msg, nil)

	var reported util.InfoMsg
	for _, c := range cmds {
		if c == nil {
			continue
		}
		if info, ok := c().(util.InfoMsg); ok && info.Type == util.InfoTypeError {
			reported = info
			break
		}
	}
	require.Equal(t, util.InfoTypeError, reported.Type, "the cancel failure must be reported")
	require.Contains(t, reported.Msg, cancelErr.Error())

	reopened, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok, "the form must be reopened so the cancel can be retried")
	require.Equal(t, batch.ID, reopened.BatchID)
}

// TestQuestionAnswer_NoErrorDoesNotReopenForm confirms the ordinary
// success path is untouched: no questionAnswerResultMsg is even produced
// when the call succeeds (see openBatchFormDialog's OnAnswer closure), so
// nothing here reopens or reports anything.
func TestQuestionAnswer_NoErrorDoesNotReopenForm(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)

	batch := testQuestionBatch("batch-ok")
	u.openBatchFormDialog(batch)
	form, ok := u.activeInline.(*dialog.QuestionForm)
	require.True(t, ok)

	yes := true
	cmd := form.OnAnswer([]question.Answer{{QuestionID: "q1", Yes: &yes}})
	require.NotNil(t, cmd)
	require.Nil(t, cmd(), "a successful answer must not produce a result message")
}
