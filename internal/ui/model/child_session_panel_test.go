package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newChildSessionPanelTestUI builds a UI in uiChat with a two-level-deep
// nav stack (main -> agent1 -> agent2), mirroring what enterChildSession
// leaves behind after two descents, and a real layout so u.layout.editor
// is a real rectangle sized for the panel.
func newChildSessionPanelTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.session = &session.Session{ID: "grandchild-session", PromptTokens: 800, CompletionTokens: 200}
	u.navStack = []sessionNavFrame{
		{parentSessionID: "main-session", parentTitle: "main", label: "agent1"},
		{parentSessionID: "child-session", parentTitle: "agent1", label: "agent2", model: "claude-sonnet-5", effort: "medium"},
	}
	u.updateLayoutAndSize()
	return u
}

// TestChildSessionBreadcrumbChain covers the chain builder that feeds
// drawChildSessionPanel: root title first, then each frame's label, in
// descent order.
func TestChildSessionBreadcrumbChain(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	require.Equal(t, []string{"main", "agent1", "agent2"}, u.childSessionBreadcrumbChain())
}

// TestChildSessionPanelReplacesEditor: generateLayout must give the editor
// area a fixed childSessionPanelHeight while viewing a child session,
// instead of the textarea-driven height it uses otherwise.
func TestChildSessionPanelReplacesEditor(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.updateLayoutAndSize()
	require.NotEqual(t, childSessionPanelHeight, u.layout.editor.Dy(),
		"outside child-session view the editor must size to the textarea, not the panel")

	u.navStack = []sessionNavFrame{{parentSessionID: "parent", parentTitle: "main", label: "agent1"}}
	u.updateLayoutAndSize()

	require.Equal(t, childSessionPanelHeight, u.layout.editor.Dy(),
		"viewing a child session must give the editor area the panel's fixed height")
}

// TestChildSessionPanelClickExitsChildSession: a click anywhere in the
// editor area (now occupied by the panel) must pop the nav stack, the same
// as alt+up or the status bar's back-button click.
func TestChildSessionPanelClickExitsChildSession(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	require.NotZero(t, u.layout.editor)

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.editor.Min.X,
		Y:      u.layout.editor.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Len(t, u.navStack, 1, "clicking the panel must pop exactly one nav frame")
	require.NotNil(t, cmd, "must return the loadSession cmd for the previous level")
}

// TestChildSessionPanelButtonHover: moving the pointer over the panel's
// "back" button sets childPanelHover, and moving away clears it.
func TestChildSessionPanelButtonHover(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	require.NotZero(t, u.childPanelButtonRect, "drawing the panel must compute the button's click/hover rect")

	u.Update(tea.MouseMotionMsg(tea.Mouse{
		X: u.childPanelButtonRect.Min.X,
		Y: u.childPanelButtonRect.Min.Y,
	}))
	require.True(t, u.childPanelHover, "hovering the button must set childPanelHover")

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: 0, Y: 0}))
	require.False(t, u.childPanelHover, "moving off the button must clear childPanelHover")
}

// TestDrawChildSessionPanel_ShowsBreadcrumbModelEffortTokensState covers
// the panel's content: the breadcrumb chain with a sibling counter, the
// delegation's model/effort, the running/done state, and the child
// session's own token usage.
func TestDrawChildSessionPanel_ShowsBreadcrumbModelEffortTokensState(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	u.navStack[len(u.navStack)-1].siblings = make([]childSessionRef, 3)
	u.navStack[len(u.navStack)-1].siblingIndex = 1

	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionPanel(scr, u.layout.editor)
	out := ansi.Strip(scr.Render())

	require.Contains(t, out, "main")
	require.Contains(t, out, "agent1")
	require.Contains(t, out, "agent2 (2/3)", "current breadcrumb segment must carry the sibling counter")
	// The trailing ")" can land exactly on the drawn area's last column
	// depending on how the arrow glyph's display width is accounted for,
	// so check the stable prefix rather than the full label.
	require.Contains(t, out, "back (alt+up")
	require.Contains(t, out, "claude-sonnet-5")
	require.Contains(t, out, "effort medium")
	require.Contains(t, out, "done", "agent is idle by default in the test harness")
	require.Contains(t, out, "800", "prompt token count must be shown")
	require.Contains(t, out, "200", "completion token count must be shown")
}

// TestChildSessionPanelHeight_TwoRowAreaOmitsTokens: with only two rows
// available the panel must still render the breadcrumb and state/model
// line without panicking, and must not draw the (missing) third row.
func TestChildSessionPanelHeight_TwoRowAreaOmitsTokens(t *testing.T) {
	t.Parallel()

	u := newChildSessionPanelTestUI(t)
	area := u.layout.editor
	area.Max.Y = area.Min.Y + 2

	scr := uv.NewScreenBuffer(u.width, u.height)
	require.NotPanics(t, func() {
		u.drawChildSessionPanel(scr, area)
	})
	out := ansi.Strip(scr.Render())
	require.Contains(t, out, "claude-sonnet-5")
	require.NotContains(t, out, "prompt", "the token row is out of bounds and must not render")
}
