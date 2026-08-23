package dialog

import (
	"fmt"
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestConfirmComponentCompositorClearsWhenButtonsScrollOutOfView is the
// regression test for the button-hit compositor keeping stale
// coordinates: Draw only rebuilds c.compositor when the button row is
// inside the visible window. If a later Draw scrolls the button row out
// of view, the old code left the compositor pointing at last frame's
// (now-reused-for-other-content) coordinates, so a click there could
// still activate the invisible "Yup!" button.
func TestConfirmComponentCompositorClearsWhenButtonsScrollOutOfView(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()
	labels := make([]string, 20)
	for i := range labels {
		labels[i] = fmt.Sprintf("Question %d", i+1)
	}
	c := NewConfirmComponent(&s, "Confirm", "", labels, nil, nil)

	area := image.Rect(0, 0, 40, 5) // narrow viewport: content overflows
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())

	// Scroll to the bottom so the button row is visible and gets a
	// compositor built at these coordinates.
	for range 100 {
		c.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	c.Draw(scr, area)
	require.NotNil(t, c.compositor, "buttons should be visible at the bottom of the scroll")

	// Find a point that hits the "Yup!" button (index 0) at this scroll
	// position.
	var clickX, clickY int
	found := false
	for y := area.Min.Y; y < area.Max.Y && !found; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			if idx := hitButtonIndexForTest(c, x, y); idx == 0 {
				clickX, clickY = x, y
				found = true
				break
			}
		}
	}
	require.True(t, found, "expected to locate the Yup! button on screen")

	// Scroll back to the top: the button row is no longer in the
	// viewport.
	c.scrollOffset = 0
	c.Draw(scr, area)

	var confirmed bool
	c.OnConfirm = func() { confirmed = true }

	done, handled := c.HandleMouseClick(clickX, clickY)
	require.False(t, handled, "a click where the button used to be must not hit anything once it has scrolled away")
	require.False(t, done)
	require.False(t, confirmed, "the stale button position must not confirm the batch")
}

// hitButtonIndexForTest exposes common.HitButtonIndex through the
// component's current compositor for the search loop above.
func hitButtonIndexForTest(c *ConfirmComponent, x, y int) int {
	if c.compositor == nil {
		return -1
	}
	hit := c.compositor.Hit(x, y)
	if hit.Empty() {
		return -1
	}
	var idx int
	if _, err := fmt.Sscanf(hit.ID(), "btn_%d", &idx); err != nil {
		return -1
	}
	return idx
}
