package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/ui/util"
	"github.com/stretchr/testify/require"
)

// newBackButtonTestUI builds a UI in uiChat with a real layout (so
// u.layout.status is a real rectangle, not the zero value) and a
// child-session nav frame already pushed, mirroring what
// enterChildSession leaves behind.
func newBackButtonTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.session = &session.Session{ID: "child-session"}
	u.navStack = []sessionNavFrame{{parentSessionID: "parent-session", parentTitle: "My Parent Session"}}
	breadcrumb := childSessionBreadcrumb("My Parent Session", "subagent", 0, 1)
	u.status.SetInfoMsg(util.InfoMsg{Type: util.InfoTypeInfo, Msg: breadcrumb})
	u.status.SetChildSessionBack(true)
	u.updateLayoutAndSize()
	return u
}

// TestChildSessionBackBannerRendersAndIsClickable is the render+click
// regression test for the "no visible way back except alt+up" gap: the
// status bar must show a "← back to ..." banner while viewing a child
// session, styled as clickable (underlined), and a left click anywhere on
// the status bar in that state must pop the nav stack — same as alt+up.
func TestChildSessionBackBannerRendersAndIsClickable(t *testing.T) {
	t.Parallel()

	u := newBackButtonTestUI(t)
	require.True(t, u.status.IsChildSessionBack())
	require.Contains(t, u.status.InfoMsg().Msg, "← back to My Parent Session")

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.status.Min.X,
		Y:      u.layout.status.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Empty(t, u.navStack, "clicking the back banner must pop the nav stack, same as alt+up")
	require.NotNil(t, cmd, "must return the loadSession cmd for the parent")
}

// TestChildSessionBackBannerHoverFeedback covers the hover cue: moving the
// pointer over the status bar while the banner is active sets backHover,
// and moving away clears it.
func TestChildSessionBackBannerHoverFeedback(t *testing.T) {
	t.Parallel()

	u := newBackButtonTestUI(t)

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: u.layout.status.Min.X, Y: u.layout.status.Min.Y}))
	require.True(t, u.status.backHover, "hovering the status bar while the banner is active must set backHover")

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: u.layout.status.Min.X, Y: u.layout.status.Min.Y - 5}))
	require.False(t, u.status.backHover, "moving off the status bar must clear backHover")
}

// TestChildSessionBackBannerInactiveClickIgnored: a click on the status
// bar when no child session is being viewed must not be intercepted —
// the status bar has no other mouse handling, so this just confirms the
// new branch is properly gated.
func TestChildSessionBackBannerInactiveClickIgnored(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.session = &session.Session{ID: "top-level"}
	u.updateLayoutAndSize()

	require.False(t, u.status.IsChildSessionBack())
	require.NotPanics(t, func() {
		u.Update(tea.MouseClickMsg(tea.Mouse{
			X:      u.layout.status.Min.X,
			Y:      u.layout.status.Min.Y,
			Button: uv.MouseLeft,
		}))
	})
}
