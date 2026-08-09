package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

type attachmentClickWorkspace struct {
	historyWorkspace
}

func (attachmentClickWorkspace) AgentIsReady() bool { return false }

func newAttachmentClickTestUI(t *testing.T) (*UI, int) {
	t.Helper()

	u := newTestUI()
	u.com.Workspace = attachmentClickWorkspace{}
	u.dialog = dialog.NewOverlay()
	sty := u.com.Styles.Attachments
	renderer := attachments.NewRenderer(
		sty.Normal,
		sty.Deleting,
		sty.Image,
		sty.Text,
		sty.Skill,
		sty.Remove,
	)
	u.editor.attachments = attachments.New(renderer, attachments.Keymap{})
	u.updateLayoutAndSize()
	require.True(t, u.editor.attachments.Update(message.Attachment{FileName: "test.txt"}))
	_ = u.editor.attachments.Render(u.layout.editor.Dx())

	for x := range u.layout.editor.Dx() {
		if renderer.HitTestRemove(u.editor.attachments.List(), x) == 0 {
			return u, x
		}
	}
	t.Fatal("remove button was not rendered")
	return nil, 0
}

func TestAttachmentClickIgnoredWhileInlineEditorIsActive(t *testing.T) {
	t.Parallel()

	u, removeX := newAttachmentClickTestUI(t)
	u.openBatchFormDialog(question.Request{
		Questions: []question.Question{{
			ID:   "question",
			Type: question.TypeFreeText,
			Text: "Question?",
		}},
	})

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.editor.Min.X + removeX,
		Y:      u.layout.editor.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Len(t, u.editor.attachments.List(), 1)
}

func TestAttachmentClickRequiresLeftMouseButton(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		button    tea.MouseButton
		remaining int
	}{
		{name: "left", button: uv.MouseLeft, remaining: 0},
		{name: "middle", button: uv.MouseMiddle, remaining: 1},
		{name: "right", button: uv.MouseRight, remaining: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			u, removeX := newAttachmentClickTestUI(t)
			_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{
				X:      u.layout.editor.Min.X + removeX,
				Y:      u.layout.editor.Min.Y,
				Button: tt.button,
			}))

			require.Len(t, u.editor.attachments.List(), tt.remaining)
		})
	}
}
