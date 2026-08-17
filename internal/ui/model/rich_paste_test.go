package model

import (
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// TestRichPasteAttachesImagesAndInsertsText covers the mixed-clipboard case:
// a browser selection carrying both prose and images lands as text in the
// editor plus one attachment per image.
func TestRichPasteAttachesImagesAndInsertsText(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor

	u.handleRichPaste(richPasteMsg{
		text: "look at this",
		attachments: []message.Attachment{
			{FileName: "paste_1.png", FilePath: "paste_1.png", MimeType: "image/png", Content: []byte("a")},
			{FileName: "paste_2.png", FilePath: "paste_2.png", MimeType: "image/png", Content: []byte("b")},
		},
	})

	require.Equal(t, "look at this", u.editor.textarea.Value())
	list := u.editor.attachments.List()
	require.Len(t, list, 2)
	require.Equal(t, "paste_1.png", list[0].FileName)
	require.Equal(t, "paste_2.png", list[1].FileName)
}

// TestRichPasteWithoutTextStillAttaches guards the image-only rich payload:
// nothing is typed into the editor, but the images are kept.
func TestRichPasteWithoutTextStillAttaches(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor

	u.handleRichPaste(richPasteMsg{
		text:        "   \n",
		attachments: []message.Attachment{{FileName: "paste_1.png", MimeType: "image/png"}},
	})

	require.Empty(t, u.editor.textarea.Value())
	require.Len(t, u.editor.attachments.List(), 1)
}

// TestRichPasteLongTextBecomesAnAttachment shows the text half going through
// the regular paste path, thresholds included.
func TestRichPasteLongTextBecomesAnAttachment(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	u.focus = uiFocusEditor

	cmd := u.handleRichPaste(richPasteMsg{
		text:        strings.Repeat("line\n", pasteLinesThreshold+5),
		attachments: []message.Attachment{{FileName: "paste_1.png", MimeType: "image/png"}},
	})

	require.Empty(t, u.editor.textarea.Value(), "an oversized paste is attached, not typed")
	require.NotNil(t, cmd)
	require.Len(t, u.editor.attachments.List(), 1)
}

// TestPasteIdxCountsImageAttachments keeps successive pastes from reusing a
// name: image attachments advance the counter the same way text ones do.
func TestPasteIdxCountsImageAttachments(t *testing.T) {
	t.Parallel()

	u := newSlashTestUI(t)
	require.Equal(t, 1, u.pasteIdx())

	u.editor.attachments.Update(message.Attachment{FileName: "paste_3.png"})
	require.Equal(t, 4, u.pasteIdx())

	u.editor.attachments.Update(message.Attachment{FileName: "paste_7.txt"})
	require.Equal(t, 8, u.pasteIdx())
}

// TestPasteHelperNoticeWarnsWhenNoHelperIsInstalled covers the case that
// used to be silent: Ctrl+V on a browser selection with no clipboard helper
// around, where the images are gone before Sennit ever sees them.
func TestPasteHelperNoticeWarnsWhenNoHelperIsInstalled(t *testing.T) {
	t.Setenv("PATH", "")

	msg := pasteHelperNotice()

	info, ok := msg.(util.InfoMsg)
	require.True(t, ok, "expected an InfoMsg, got %T", msg)
	require.Equal(t, util.InfoTypeWarn, info.Type)
	require.Contains(t, info.Msg, "paste images")
}
