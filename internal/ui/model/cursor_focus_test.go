package model

import (
	"context"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
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
		{"sidebar", uiFocusSidebar},
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

// TestFocusSidebarGuard_DoesNotSwallowPillNavigation is a regression test
// for a keybinding collision introduced when pills navigation (PillLeft/
// PillRight, bound to left/right) was added reusing the same left/right/"h"/
// "l" keys as the pre-existing FocusSidebar/FocusChat bindings (see
// key.NewBinding for Chat.FocusSidebar in keys.go). uiFocusMain matched
// FocusSidebar's key first and only checked its eligibility (non-compact,
// scrollable sidebar, has session) inside the case body; when that guard
// failed the case had already claimed the keypress, so it never reached the
// PillRight handling in handleGlobalKeys — right arrow silently did
// nothing instead of switching the pill section. The guard now lives in the
// case predicate so an ineligible FocusSidebar falls through instead of
// swallowing the key.
func TestFocusSidebarGuard_DoesNotSwallowPillNavigation(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.focus = uiFocusMain
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "do work"},
	}
	u.wsCache.promptQueue = 2
	u.pills.expanded = true
	u.pills.focusedSection = pillSectionTodos
	// forceCompactMode (set by newCursorTestUI) makes FocusSidebar
	// ineligible (!m.isCompact is one of its guard conditions), which is
	// exactly the scenario that used to swallow the key.
	u.updateLayoutAndSize()

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyRight})

	require.Equal(t, uiFocusMain, u.focus, "an ineligible FocusSidebar must not move focus")
	require.Equal(t, pillSectionQueue, u.pills.focusedSection,
		"right arrow must reach PillRight and switch the pill section")
}

// TestFocusSidebarGuard_StillClaimsRightWhenEligible confirms the fix above
// only widens PillRight's reach when FocusSidebar is not applicable; when the
// sidebar really is focusable, right arrow must still move focus there (the
// documented, longstanding behavior), not be captured by pill navigation.
func TestFocusSidebarGuard_StillClaimsRightWhenEligible(t *testing.T) {
	t.Parallel()

	u := newCursorTestUI(t)
	u.forceCompactMode = false
	u.focus = uiFocusMain
	u.session.Todos = []session.Todo{
		{Status: session.TodoStatusInProgress, Content: "do work"},
	}
	u.sidebar.scrollable = true
	u.pills.expanded = true
	u.pills.focusedSection = pillSectionTodos
	u.updateLayoutAndSize()

	u.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeyRight})

	require.Equal(t, uiFocusSidebar, u.focus, "an eligible FocusSidebar must still claim right arrow")
	require.Equal(t, pillSectionTodos, u.pills.focusedSection, "pill section must not change here")
}
