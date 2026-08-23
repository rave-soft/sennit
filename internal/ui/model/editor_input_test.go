package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/stretchr/testify/require"
)

// TestHandlePasteMsgThreshold_SnapshotsPasteIdxAtCallTime covers the
// off-goroutine access bug in handlePasteMsg's oversized-paste branch: the
// returned tea.Cmd runs on the cmd goroutine, so it must decide the
// attachment's paste_N name from the attachment count at the moment the
// paste was handled, not m.editor.attachments read again whenever the cmd
// actually executes (which races with further attachment updates).
func TestHandlePasteMsgThreshold_SnapshotsPasteIdxAtCallTime(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor
	require.Equal(t, 1, u.pasteIdx())

	cmd := u.handlePasteMsg(tea.PasteMsg{Content: strings.Repeat("line\n", pasteLinesThreshold+5)})
	require.NotNil(t, cmd)

	// Simulate another attachment being added (e.g. a concurrent paste)
	// between the cmd being built and it actually running.
	u.editor.attachments.Update(message.Attachment{FileName: "paste_9.png"})

	msg := cmd()
	attachment, ok := msg.(message.Attachment)
	require.True(t, ok, "expected a message.Attachment, got %T", msg)
	require.Equal(t, "paste_1.txt", attachment.FileName,
		"the attachment name must use the paste index captured when the paste was handled")
}

// TestPasteImageFromClipboardCmd_DoesNotReadModelOffGoroutine covers the
// off-goroutine access bug in the Ctrl+V clipboard-image path:
// pasteImageFromClipboard/pasteRichFromClipboard used to be *UI methods
// bound directly as the tea.Cmd (keypress.go: `cmds = append(cmds,
// m.pasteImageFromClipboard)`), so they read m.editor.attachments and
// m.com on the cmd goroutine while Update kept mutating them on the main
// one. pasteImageFromClipboardCmd must snapshot everything it needs
// (ctx, pasteIdx) before returning the closure, which by construction can
// no longer touch m at all.
//
// This only has teeth under -race (see racecheck_on_test.go): without the
// detector, an unsynchronized read/write pair like this usually "just
// works" and the test would pass either way, so it is skipped rather than
// giving false confidence.
func TestPasteImageFromClipboardCmd_DoesNotReadModelOffGoroutine(t *testing.T) {
	if !raceDetectorEnabled {
		t.Skip("requires -race to observe the off-goroutine model access this guards against")
	}
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor

	cmd := u.pasteImageFromClipboardCmd()
	require.NotNil(t, cmd)

	done := make(chan struct{})
	go func() {
		defer close(done)
		cmd()
	}()

	// Concurrently touch the model state the pre-fix closure used to read
	// directly while the cmd runs on its own goroutine.
	u.editor.attachments.Update(message.Attachment{FileName: "paste_1.png"})
	_ = u.com.Context()

	<-done
}

// TestOpenEditor_DoesNotTouchDiskUntilCmdRuns is the regression test for the
// $EDITOR scratch file: openEditor used to call os.CreateTemp and write the
// message body directly, on the Update goroutine, before ever returning a
// tea.Cmd. It must instead defer both to the closure it returns, so the key
// handler that calls it never touches the filesystem itself.
func TestOpenEditor_DoesNotTouchDiskUntilCmdRuns(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)

	before, err := filepath.Glob(filepath.Join(os.TempDir(), "msg_*.md"))
	require.NoError(t, err)

	cmd := u.openEditor("hello from the editor")
	require.NotNil(t, cmd)

	after, err := filepath.Glob(filepath.Join(os.TempDir(), "msg_*.md"))
	require.NoError(t, err)
	require.Equal(t, before, after, "openEditor must not create the scratch file before its cmd runs")

	msg := cmd()
	ready, ok := msg.(openEditorReadyMsg)
	require.True(t, ok, "expected openEditorReadyMsg, got %T", msg)
	t.Cleanup(func() { _ = os.Remove(ready.tmpPath) })

	content, err := os.ReadFile(ready.tmpPath)
	require.NoError(t, err)
	require.Equal(t, "hello from the editor", string(content),
		"the scratch file must carry the message body once the cmd has run")
	require.NotNil(t, ready.cmd, "a prepared exec.Cmd must be returned so Update can launch it")
}

// TestQuestionForm_OnAnswerAndOnCancelDeferToCmd is the regression test for
// the question form's submit/cancel path: openBatchFormDialog used to wire
// OnAnswer/OnCancel to call ws.QuestionAnswer/ws.QuestionCancel directly,
// which HandleKey then invoked inline on the Update goroutine — a channel
// send into the question service. Both must now return a tea.Cmd instead,
// so HandleKey only builds the command and Update runs it, and the answer
// still reaches the workspace once that cmd runs.
func TestQuestionForm_OnAnswerAndOnCancelDeferToCmd(t *testing.T) {
	t.Parallel()

	ws := &cmdDrivingWorkspace{agentReady: true}
	u := newCmdDrivenUI(ws)
	u.openBatchFormDialog(question.Request{
		Questions: []question.Question{{
			ID:   "q1",
			Type: question.TypeYesNo,
			Text: "Ready?",
		}},
	})

	done, cmd := u.activeInline.HandleKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	require.True(t, done, "answering the only question in a single-question form must submit")
	require.NotNil(t, cmd, "submit must hand back a cmd instead of calling the workspace inline")
	require.Zero(t, ws.questionAnswerCalls, "OnAnswer must not call the workspace before its cmd runs")

	cmd()
	require.Equal(t, 1, ws.questionAnswerCalls, "the answer must still reach the workspace once the cmd runs")
	require.Len(t, ws.questionAnswerResponse, 1)
	require.Equal(t, "q1", ws.questionAnswerResponse[0].QuestionID)
	require.NotNil(t, ws.questionAnswerResponse[0].Yes)
	require.True(t, *ws.questionAnswerResponse[0].Yes)

	// Cancelling a fresh form follows the same contract.
	u.openBatchFormDialog(question.Request{
		Questions: []question.Question{{
			ID:   "q1",
			Type: question.TypeYesNo,
			Text: "Ready?",
		}},
	})
	done, cancelCmd := u.activeInline.HandleKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	require.True(t, done)
	require.NotNil(t, cancelCmd, "cancel must hand back a cmd instead of calling the workspace inline")
	require.Zero(t, ws.questionCancelCalls, "OnCancel must not call the workspace before its cmd runs")

	cancelCmd()
	require.Equal(t, 1, ws.questionCancelCalls)
}
