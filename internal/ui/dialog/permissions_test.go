package dialog

import (
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func newTestPermissions(t *testing.T) *Permissions {
	t.Helper()
	s := styles.CharmtonePantera()
	com := &common.Common{Styles: &s}
	perm := permission.PermissionRequest{
		ID:         "perm-test",
		ToolCallID: "tool-call-test",
		ToolName:   "bash",
	}
	return NewPermissions(com, perm)
}

// TestPermissions_ActionKeysResolve verifies that action keys produce the
// correct permission response.
func TestPermissions_ActionKeysResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key    tea.KeyPressMsg
		action PermissionAction
	}{
		{keyMsg('a'), PermissionAllow},
		{keyMsg('A'), PermissionAllow},
		{keyMsg('d'), PermissionDeny},
		{keyMsg('D'), PermissionDeny},
		{keyMsg('s'), PermissionAllowForSession},
		{keyMsg('S'), PermissionAllowForSession},
	}

	for _, tc := range tests {
		p := newTestPermissions(t)
		action := p.HandleMsg(tc.key)
		resp, ok := action.(ActionPermissionResponse)
		require.Truef(t, ok, "key %q should produce ActionPermissionResponse", tc.key.Text)
		require.Equal(t, tc.action, resp.Action)
	}
}

// TestPermissions_NavigationCyclesOptions verifies that tab and arrow keys
// cycle through the three permission options.
func TestPermissions_NavigationCyclesOptions(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	require.Equal(t, 0, p.selectedOption)

	// Tab cycles forward.
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 1, p.selectedOption)

	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 2, p.selectedOption)

	// Wrap around.
	p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, 0, p.selectedOption)

	// Left cycles backward.
	p.HandleMsg(keyMsg('h'))
	require.Equal(t, 2, p.selectedOption)
}

// TestPermissions_EnterConfirmsSelection verifies that enter confirms the
// currently selected option.
func TestPermissions_EnterConfirmsSelection(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	p.selectedOption = 1 // Allow for session.

	action := p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionAllowForSession, resp.Action)
}

// TestPermissions_EscapeDenies verifies that escape denies the request.
func TestPermissions_EscapeDenies(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	action := p.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	resp, ok := action.(ActionPermissionResponse)
	require.True(t, ok)
	require.Equal(t, PermissionDeny, resp.Action)
}

// drawTestPermissions draws p onto a screen-sized buffer so its
// buttonRects (used for mouse hit-testing) get populated the same way
// they would in the real overlay.
func drawTestPermissions(t *testing.T, p *Permissions) {
	t.Helper()
	area := image.Rect(0, 0, 100, 40)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	p.Draw(scr, area)
}

// TestPermissions_MouseClickTriggersButtonAction verifies that clicking
// each button's rect produces the same action as its keyboard shortcut,
// and moves the keyboard selection to match.
func TestPermissions_MouseClickTriggersButtonAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		option int
		action PermissionAction
	}{
		{"Allow", 0, PermissionAllow},
		{"Allow for Session", 1, PermissionAllowForSession},
		{"Deny", 2, PermissionDeny},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := newTestPermissions(t)
			drawTestPermissions(t, p)

			rect := p.buttonRects[tc.option]
			require.False(t, rect.Empty(), "button %d should have a non-empty rect after Draw", tc.option)

			action := p.HandleMsg(tea.MouseClickMsg{X: rect.Min.X, Y: rect.Min.Y, Button: tea.MouseLeft})
			resp, ok := action.(ActionPermissionResponse)
			require.Truef(t, ok, "click on %s button should produce ActionPermissionResponse, got %#v", tc.name, action)
			require.Equal(t, tc.action, resp.Action)
			require.Equal(t, tc.option, p.selectedOption)
		})
	}
}

// TestPermissions_MouseClickIgnoresNonLeftButton verifies that a
// right-click on a button doesn't trigger its action.
func TestPermissions_MouseClickIgnoresNonLeftButton(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	drawTestPermissions(t, p)

	rect := p.buttonRects[2] // Deny
	action := p.HandleMsg(tea.MouseClickMsg{X: rect.Min.X, Y: rect.Min.Y, Button: tea.MouseRight})
	require.Nil(t, action)
}

// TestPermissions_MouseHoverMovesSelection verifies that hovering a
// button moves the keyboard-driven selection to match, without itself
// producing an action.
func TestPermissions_MouseHoverMovesSelection(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	drawTestPermissions(t, p)
	require.Equal(t, 0, p.selectedOption)

	denyRect := p.buttonRects[2]
	action := p.HandleMsg(tea.MouseMotionMsg{X: denyRect.Min.X, Y: denyRect.Min.Y})
	require.Nil(t, action)
	require.Equal(t, 2, p.selectedOption)
	require.Equal(t, 2, p.hoverIndex)

	// Moving off every button clears the hover highlight but leaves the
	// last selection (matches keyboard nav: hover only moves the
	// selection while directly over a button).
	action = p.HandleMsg(tea.MouseMotionMsg{X: 0, Y: 0})
	require.Nil(t, action)
	require.Equal(t, 2, p.selectedOption)
	require.Equal(t, -1, p.hoverIndex)
}

// TestPermissions_MouseClickOutsideButtonsIsNoOp verifies that clicking
// away from any button neither produces an action nor changes selection.
func TestPermissions_MouseClickOutsideButtonsIsNoOp(t *testing.T) {
	t.Parallel()

	p := newTestPermissions(t)
	drawTestPermissions(t, p)
	p.selectedOption = 1

	action := p.HandleMsg(tea.MouseClickMsg{X: 0, Y: 0, Button: tea.MouseLeft})
	require.Nil(t, action)
	require.Equal(t, 1, p.selectedOption)
}
