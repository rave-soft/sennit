package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/attachments"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// newChildBannerTestUI builds a UI in uiChat with a two-level-deep nav
// stack (main -> agent1 -> agent2), mirroring what enterChildSession
// leaves behind after two descents, and a real layout so
// u.layout.childBanner is a real rectangle.
func newChildBannerTestUI(t *testing.T) *UI {
	t.Helper()
	u := newTestUI()
	u.com.Workspace = agentSessionWorkspace{}
	u.dialog = dialog.NewOverlay()
	u.editor.attachments = attachments.New(nil, attachments.Keymap{})
	u.session = &session.Session{ID: "grandchild-session"}
	u.navStack = []sessionNavFrame{
		{parentSessionID: "main-session", parentTitle: "main", label: "agent1"},
		{parentSessionID: "child-session", parentTitle: "agent1", label: "agent2"},
	}
	u.updateLayoutAndSize()
	return u
}

// TestChildSessionBreadcrumbChain covers the chain builder that feeds
// drawChildSessionBanner: root title first, then each frame's label, in
// descent order.
func TestChildSessionBreadcrumbChain(t *testing.T) {
	t.Parallel()

	u := newChildBannerTestUI(t)
	require.Equal(t, []string{"main", "agent1", "agent2"}, u.childSessionBreadcrumbChain())
}

// TestChildSessionBannerReservesTopRowOfMain: generateLayout must carve a
// one-row childBanner off the top of main while viewing a child session,
// and leave it empty (zero Rectangle) otherwise.
func TestChildSessionBannerReservesTopRowOfMain(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.updateLayoutAndSize()
	require.Zero(t, u.layout.childBanner, "no banner row outside child-session view")
	mainHeightNoChild := u.layout.main.Dy()

	u.navStack = []sessionNavFrame{{parentSessionID: "parent", parentTitle: "main", label: "agent1"}}
	u.updateLayoutAndSize()

	require.Equal(t, 1, u.layout.childBanner.Dy(), "banner must be exactly one row tall")
	require.Equal(t, mainHeightNoChild-1, u.layout.main.Dy(),
		"the banner row must come out of main, not be added on top of it")
}

// TestChildSessionBannerClickExitsChildSession: a click anywhere in the
// banner area must pop the nav stack, the same as alt+up or the status
// bar's back-button click.
func TestChildSessionBannerClickExitsChildSession(t *testing.T) {
	t.Parallel()

	u := newChildBannerTestUI(t)
	require.NotZero(t, u.layout.childBanner)

	_, cmd := u.Update(tea.MouseClickMsg(tea.Mouse{
		X:      u.layout.childBanner.Min.X,
		Y:      u.layout.childBanner.Min.Y,
		Button: uv.MouseLeft,
	}))

	require.Len(t, u.navStack, 1, "clicking the banner must pop exactly one nav frame")
	require.NotNil(t, cmd, "must return the loadSession cmd for the previous level")
}

// TestChildSessionBannerButtonHover: moving the pointer over the banner's
// "exit up" button sets childBannerHover, and moving away clears it.
func TestChildSessionBannerButtonHover(t *testing.T) {
	t.Parallel()

	u := newChildBannerTestUI(t)
	scr := uv.NewScreenBuffer(u.width, u.height)
	u.drawChildSessionBanner(scr, u.layout.childBanner)
	require.NotZero(t, u.childBannerButtonRect, "drawing the banner must compute the button's click/hover rect")

	u.Update(tea.MouseMotionMsg(tea.Mouse{
		X: u.childBannerButtonRect.Min.X,
		Y: u.childBannerButtonRect.Min.Y,
	}))
	require.True(t, u.childBannerHover, "hovering the button must set childBannerHover")

	u.Update(tea.MouseMotionMsg(tea.Mouse{X: 0, Y: 0}))
	require.False(t, u.childBannerHover, "moving off the button must clear childBannerHover")
}
