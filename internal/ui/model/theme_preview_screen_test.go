package model

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// TestThemePreview_RepaintsWholeScreen drives the picker the way a user
// does — open it, walk down a row, back out — and looks at the whole
// rendered frame rather than at individual widgets. That is the point of
// the feature (the preview is the real UI in the new palette, dialog
// included) and also the strongest available check that no corner of the
// screen is left in the old colors: anything that missed the switch would
// keep the frame from returning to its original bytes on cancel.
func TestThemePreview_RepaintsWholeScreen(t *testing.T) {
	ws := &cmdDrivingWorkspace{}
	m := newCmdDrivenGoldenUI(ws)

	m.openDialog(dialog.ThemeID)
	require.True(t, m.dialog.ContainsDialog(dialog.ThemeID))
	opened := string(renderCmdDrivenUI(m))

	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	previewed := string(renderCmdDrivenUI(m))
	require.NotEqual(t, opened, previewed, "moving through the picker must repaint the screen")

	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.False(t, m.dialog.ContainsDialog(dialog.ThemeID))

	m.openDialog(dialog.ThemeID)
	require.Equal(t, opened, string(renderCmdDrivenUI(m)),
		"abandoning the preview must restore the palette it started in")
}
