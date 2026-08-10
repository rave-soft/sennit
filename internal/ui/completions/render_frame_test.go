package completions

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// framedStyles returns distinguishable styles so border/highlight/scrollbar
// presence can be asserted on the rendered output rather than guessed at.
func framedStyles() PopupStyles {
	return PopupStyles{
		Normal:         lipgloss.NewStyle(),
		Focused:        lipgloss.NewStyle().Reverse(true),
		Match:          lipgloss.NewStyle().Bold(true),
		Muted:          lipgloss.NewStyle().Faint(true),
		Border:         lipgloss.NewStyle().Border(lipgloss.RoundedBorder()),
		ScrollbarThumb: lipgloss.NewStyle(),
		ScrollbarTrack: lipgloss.NewStyle(),
	}
}

// TestRenderDrawsRoundedBorderFrame covers the "solid panel, not a bare
// list" redesign: Render() must wrap the list in the configured border
// (top/bottom border runes present, plain rows do not have them), and
// Size() must report the border-inclusive footprint that Render() actually
// produces.
func TestRenderDrawsRoundedBorderFrame(t *testing.T) {
	t.Parallel()

	c := New(framedStyles())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new", Description: "start a new session"},
		{ID: "compact", Title: "compact", Description: "summarize the session"},
	})

	rendered := c.Render()
	lines := strings.Split(rendered, "\n")
	require.GreaterOrEqual(t, len(lines), 4, "border adds a top and bottom line around the 2 item rows")

	// Rounded border corners on the first/last line.
	require.Contains(t, ansi.Strip(lines[0]), "╭")
	require.Contains(t, ansi.Strip(lines[0]), "╮")
	require.Contains(t, ansi.Strip(lines[len(lines)-1]), "╰")
	require.Contains(t, ansi.Strip(lines[len(lines)-1]), "╯")

	w, h := c.Size()
	require.Equal(t, len(lines), h)
	for _, line := range lines {
		require.Equal(t, w, ansi.StringWidth(line), "every frame line must span the reported popup width")
	}
}

// TestRenderHighlightsSelectedRowFullWidth covers "full-width highlight,
// not just colored text": the focused/selected row must render with its
// background fill spanning the whole row, i.e. the Focused style's
// rendering of a full-width blank line must appear as a literal substring
// (the row content plus trailing padding, all under one style run).
func TestRenderHighlightsSelectedRowFullWidth(t *testing.T) {
	t.Parallel()

	c := New(framedStyles())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new"},
		{ID: "compact", Title: "compact"},
	})

	rendered := c.Render()
	// The first item is selected by default. Its row, including the
	// padding out to the popup's content width, must be wrapped in the
	// Focused style (Reverse, per framedStyles) — not just the "new" text.
	selectedRow := lipgloss.NewStyle().Reverse(true).Padding(0, 1).Width(c.width).Render("new")
	require.Contains(t, rendered, selectedRow)
}

// TestRenderShowsScrollbarWhenOverflowing covers the scroll indicator: with
// more items than fit in the popup's max height, a scrollbar column must
// appear, and Size()'s width must include it. With everything visible, no
// scrollbar column is added.
func TestRenderShowsScrollbarWhenOverflowing(t *testing.T) {
	t.Parallel()

	c := New(framedStyles())

	values := make([]CommandCompletionValue, 0, maxHeight+5)
	for i := range maxHeight + 5 {
		values = append(values, CommandCompletionValue{ID: string(rune('a' + i)), Title: string(rune('a' + i))})
	}
	c.OpenCommands(values)

	require.True(t, c.hasScrollbar(), "more items than the popup's max height must need a scrollbar")

	rendered := c.Render()
	lines := strings.Split(rendered, "\n")
	// Every row must have a scrollbar column (thumb "┃" or track "│")
	// just inside the right border.
	for _, line := range lines[1 : len(lines)-1] {
		stripped := ansi.Strip(line)
		require.True(t,
			strings.Contains(stripped, scrollbarThumbGlyph) || strings.Contains(stripped, scrollbarTrackGlyph),
			"expected a scrollbar glyph in row %q", stripped)
	}

	w, _ := c.Size()
	require.Equal(t, c.width+1+borderFrameWidth, w, "scrollbar column must be included in the reported size")
}

// TestRenderNoScrollbarWhenEverythingFits is the negative case: a handful
// of items that all fit inside the popup's height need no scrollbar, and
// Size() must not reserve a column for one.
func TestRenderNoScrollbarWhenEverythingFits(t *testing.T) {
	t.Parallel()

	c := New(framedStyles())
	c.OpenCommands([]CommandCompletionValue{
		{ID: "new_session", Title: "new"},
		{ID: "compact", Title: "compact"},
	})

	require.False(t, c.hasScrollbar())

	rendered := c.Render()
	require.NotContains(t, ansi.Strip(rendered), scrollbarThumbGlyph)

	w, _ := c.Size()
	require.Equal(t, c.width+borderFrameWidth, w)
}
