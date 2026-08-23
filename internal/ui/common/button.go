package common

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// ButtonOpts defines the configuration for a single button
type ButtonOpts struct {
	// Text is the button label
	Text string
	// UnderlineIndex is the 0-based index of the character to underline (-1 for none)
	UnderlineIndex int
	// Selected indicates whether this button is currently selected
	Selected bool
	// Hovered indicates whether the mouse is hovering over the button
	Hovered bool
	// Padding inner horizontal padding defaults to 2 if this is 0
	Padding int
}

// Button creates a button with an underlined character and selection state
func Button(t *styles.Styles, opts ButtonOpts) string {
	// Select style based on selection/hover state.
	style := t.Button.Blurred
	if opts.Selected && opts.Hovered {
		style = t.Button.Focused.Bold(true)
	} else if opts.Hovered {
		style = t.Button.Hovered.Bold(true)
	} else if opts.Selected {
		style = t.Button.Focused
	}

	text := opts.Text
	if opts.Padding == 0 {
		opts.Padding = 2
	}

	// UnderlineIndex counts characters, not bytes: len(text) counts UTF-8
	// bytes, so this bound check let a multi-byte-prefixed label pass a
	// character index that was actually past the end of the text (or
	// rejected valid ones), and the Range built below is in cell-width
	// units, which only equal byte offsets for single-byte ASCII.
	runeCount := utf8.RuneCountInString(text)
	if opts.UnderlineIndex > -1 && opts.UnderlineIndex > runeCount-1 {
		opts.UnderlineIndex = -1
	}

	// Resolve the underlined rune's cell-width column against the plain
	// label — the Range StyleRanges applies below is measured in cell
	// widths, so this has to happen before Render wraps the text in
	// style codes that would shift byte offsets without shifting
	// columns.
	var underlineStart, underlineEnd int
	if opts.UnderlineIndex != -1 {
		underlineStart, underlineEnd = underlineColumnRange(text, opts.UnderlineIndex)
	}

	text = style.Padding(0, opts.Padding).Render(text)

	if opts.UnderlineIndex != -1 {
		text = lipgloss.StyleRanges(text, lipgloss.NewRange(opts.Padding+underlineStart, opts.Padding+underlineEnd, style.Underline(true)))
	}

	return text
}

// underlineColumnRange returns the cell-width [start, end) column range of
// the idx-th rune (0-based) in text, suitable for [lipgloss.NewRange]. Using
// a byte offset here would misplace the underline on any label whose
// underlined character sits after a multi-byte rune.
func underlineColumnRange(text string, idx int) (start, end int) {
	for i, r := range text {
		if idx == 0 {
			s := text[i : i+utf8.RuneLen(r)]
			start = ansi.StringWidth(text[:i])
			return start, start + ansi.StringWidth(s)
		}
		idx--
	}
	return 0, 0
}

// ButtonGroup creates a row of selectable buttons
// Spacing is the separator between buttons
// Use "  " or similar for horizontal layout
// Use "\n"  for vertical layout
// Defaults to "  " (horizontal)
func ButtonGroup(t *styles.Styles, buttons []ButtonOpts, spacing string) string {
	if len(buttons) == 0 {
		return ""
	}

	if spacing == "" {
		spacing = "  "
	}

	parts := make([]string, len(buttons))
	for i, button := range buttons {
		parts[i] = Button(t, button)
	}

	return strings.Join(parts, spacing)
}

// ButtonHitCompositor builds a lipgloss Compositor with one hit
// layer per button, positioned horizontally at (x, y). Layer IDs
// are "btn_0", "btn_1", etc. The spacing parameter must match
// what was passed to ButtonGroup when rendering.
func ButtonHitCompositor(sty *styles.Styles, opts []ButtonOpts, spacing string, x, y int) *lipgloss.Compositor {
	if len(opts) == 0 {
		return nil
	}
	if spacing == "" {
		spacing = "  "
	}
	spacingWidth := lipgloss.Width(spacing)
	var layers []*lipgloss.Layer
	bx := x
	for i, o := range opts {
		b := Button(sty, o)
		w := lipgloss.Width(b)
		hitStr := strings.Repeat(" ", w)
		layers = append(layers, lipgloss.NewLayer(hitStr).X(bx).Y(y).ID(fmt.Sprintf("btn_%d", i)))
		bx += w + spacingWidth
	}
	return lipgloss.NewCompositor(layers...)
}

// HitButtonIndex checks a compositor for a button hit and returns
// the button index, or -1 if no button was hit.
func HitButtonIndex(c *lipgloss.Compositor, x, y int) int {
	if c == nil {
		return -1
	}
	hit := c.Hit(x, y)
	if hit.Empty() {
		return -1
	}
	var idx int
	if _, err := fmt.Sscanf(hit.ID(), "btn_%d", &idx); err != nil {
		return -1
	}
	return idx
}
