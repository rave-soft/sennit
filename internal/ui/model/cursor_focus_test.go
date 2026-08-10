package model

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// cursorTestWorkspace is a minimal workspace.Workspace stub with just enough
// wired up (a non-nil Config and WorkingDir) for a full uiChat Draw() pass:
// the header renders session/model details from Config, and View() reads
// WorkingDir for the window title.
type cursorTestWorkspace struct {
	*countingWorkspace
	cfg *config.Config
}

func (w *cursorTestWorkspace) Config() *config.Config { return w.cfg }
func (w *cursorTestWorkspace) WorkingDir() string     { return "/tmp" }

// newCursorTestUI builds a *UI in uiChat with an active session, focused
// editor, and compact mode forced on so Draw() renders the header instead of
// the virtual-scrolling sidebar (which needs a much heavier workspace stub).
// Compact mode does not affect the focus/cursor invariant under test: the
// cursor gating in Draw is keyed on m.focus, not m.isCompact.
func newCursorTestUI(t *testing.T) *UI {
	t.Helper()

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents:    map[string]config.Agent{},
	}
	ws := &cursorTestWorkspace{countingWorkspace: &countingWorkspace{ready: true}, cfg: cfg}
	com := common.DefaultCommon(context.Background(), ws)

	ta := textarea.New()
	ta.SetStyles(com.Styles.Editor.Textarea)
	ta.ShowLineNumbers = false
	ta.CharLimit = -1
	ta.SetVirtualCursor(false)
	ta.DynamicHeight = true
	ta.MinHeight = TextareaMinHeight
	ta.MaxHeight = TextareaMaxHeight
	ta.Focus()

	u := &UI{
		com:    com,
		status: NewStatus(com, nil),
		chat:   NewChat(com, config.ScrollbarDefault),
		editor: editorState{
			textarea:    ta,
			attachments: attachments.New(nil, attachments.Keymap{}),
		},
		state:            uiChat,
		focus:            uiFocusEditor,
		width:            140,
		height:           45,
		keyMap:           DefaultKeyMap(),
		dialog:           dialog.NewOverlay(),
		header:           newHeader(com),
		forceCompactMode: true,
	}
	u.status.helpKm = u
	u.chat.Focus()
	u.session = &session.Session{ID: "s1"}
	u.updateLayoutAndSize()
	return u
}

func (u *UI) drawForCursor() *tea.Cursor {
	canvas := uv.NewScreenBuffer(u.width, u.height)
	return u.Draw(canvas, canvas.Bounds())
}

// stubCursorDialog is a [dialog.Dialog] that always draws a fixed, easily
// recognizable cursor, so a test can prove the dialog owns the cursor
// outright while it is open, regardless of the UI's own focus state.
type stubCursorDialog struct{}

func (stubCursorDialog) ID() string                      { return "stub-cursor" }
func (stubCursorDialog) HandleMsg(tea.Msg) dialog.Action { return nil }
func (stubCursorDialog) Draw(uv.Screen, uv.Rectangle) *tea.Cursor {
	return &tea.Cursor{Position: tea.Position{X: 99, Y: 99}}
}

// TestDrawCursor_VisibleOnlyWhenEditorFocused pins the rule from
// internal/ui/AGENTS.md ("Focus state determines key event routing"): the
// terminal cursor must be visible if and only if a real text input owns
// focus. In the main UI that is the prompt editor (uiFocusEditor); every
// other focus target (chat/main, sidebar, or no focus at all) shows no
// panels with text input, so the cursor must be hidden rather than left
// pointing at wherever it last was.
func TestDrawCursor_VisibleOnlyWhenEditorFocused(t *testing.T) {
	t.Parallel()

	states := []struct {
		name  string
		focus uiFocusState
	}{
		{"none", uiFocusNone},
		{"editor", uiFocusEditor},
		{"main-chat", uiFocusMain},
	}

	for _, dialogOpen := range []bool{false, true} {
		for _, st := range states {
			t.Run(map[bool]string{false: "no-dialog", true: "dialog-open"}[dialogOpen]+"/"+st.name, func(t *testing.T) {
				t.Parallel()
				u := newCursorTestUI(t)
				u.focus = st.focus
				if dialogOpen {
					u.dialog = dialog.NewOverlay(stubCursorDialog{})
				}
				u.updateLayoutAndSize()

				cur := u.drawForCursor()

				switch {
				case dialogOpen:
					// A dialog is modal: it owns the cursor outright,
					// independent of the UI's own focus state underneath.
					require.NotNil(t, cur, "an open dialog must control the cursor")
					require.Equal(t, 99, cur.X)
					require.Equal(t, 99, cur.Y)
				case st.focus == uiFocusEditor:
					require.NotNil(t, cur, "the prompt editor is a real text input and must show a cursor")
				default:
					require.Nil(t, cur, "no text input owns focus, so the cursor must be hidden")
				}
			})
		}
	}
}

// TestPillNavigation_RightArrowSwitchesSection is a regression test for a
// keybinding collision that used to exist between pills navigation
// (PillLeft/PillRight, bound to left/right) and a since-removed
// FocusSidebar binding that shared the same keys: FocusSidebar matched
// first and swallowed the keypress whenever its eligibility guard failed,
// so right arrow silently did nothing instead of switching the pill
// section. The sidebar can no longer take keyboard focus at all (see
// uiFocusState), so PillRight is now the only handler left for right
// arrow in uiFocusMain.
func TestPillNavigation_RightArrowSwitchesSection(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.focus = uiFocusMain
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "do work"},
	}
	u.wsCache.promptQueue = 2
	u.pills.expanded = true
	u.pills.focusedSection = pillSectionTodos
	u.updateLayoutAndSize()

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyRight})

	require.Equal(t, uiFocusMain, u.focus, "right arrow must never move focus off the chat pane")
	require.Equal(t, pillSectionQueue, u.pills.focusedSection,
		"right arrow must reach PillRight and switch the pill section")
}

// TestClickOnPlainToolItem_DoesNotMoveFocus is the regression test for the
// "фокус/курсор уходит туда" complaint: clicking a plain tool call (view,
// bash, grep, ...) used to both expand it AND steal focus from the editor
// (any click inside the chat pane focuses uiFocusMain, which hides the
// terminal cursor — see TestDrawCursor_VisibleOnlyWhenEditorFocused).
// Click-to-expand is gone entirely now (see PlainToolItemAt), and a click
// on such a row must leave focus — and so the cursor — right where it was.
func TestClickOnPlainToolItem_DoesNotMoveFocus(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.focus = uiFocusEditor
	u.chat.Blur()
	u.editor.textarea.Focus()

	item := chat.NewToolMessageItem(u.com.Styles, "msg1",
		message.ToolCall{ID: "tc-bash", Name: "bash", Input: `{}`, Finished: true}, nil, false, nil)
	u.chat.AppendMessages(item)
	u.updateLayoutAndSize()

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.main.Min.X,
		Y:      u.layout.main.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Equal(t, uiFocusEditor, u.focus, "clicking a plain tool item must not move focus off the editor")
	require.True(t, u.editor.textarea.Focused(), "the editor must stay focused (and so keep the terminal cursor)")
}

// TestClickOnAssistantText_DoesNotMoveFocus pins the "mouse never changes
// focus" invariant from uiFocusState's doc comment: clicking ordinary chat
// content (not a plain tool call) used to move focus into the chat pane
// (uiFocusMain), hiding the terminal cursor. Selection/copy still works via
// Chat.HandleMouseDown regardless of m.focus, so there's nothing left that
// requires the click to steal focus.
func TestClickOnAssistantText_DoesNotMoveFocus(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.focus = uiFocusEditor
	u.chat.Blur()
	u.editor.textarea.Focus()

	item := chat.NewAssistantMessageItem(u.com.Styles, &message.Message{
		ID:   "m1",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "hello there"},
		},
	})
	u.chat.AppendMessages(item)
	u.updateLayoutAndSize()

	_, _ = u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.main.Min.X,
		Y:      u.layout.main.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Equal(t, uiFocusEditor, u.focus, "clicking chat content must not move focus off the editor")
	require.True(t, u.editor.textarea.Focused(), "the editor must stay focused")
}
