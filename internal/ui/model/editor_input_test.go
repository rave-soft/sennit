package model

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
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
