package diffview

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// TestTruncateCode_EllipsisStyledWithLineStyle is a regression test for
// truncateCode (used by both renderUnified and renderSplit in place of a
// bare ansi.Truncate(content, width, "…") call). ansi.Truncate pastes its
// tail in using whatever SGR state happens to be active at the cut
// point; xchroma.Formatter emits some tokens — notably runs of
// whitespace with no chroma style entry — as completely bare text with
// no escape codes, so a cut landing there left the ellipsis with no
// styling at all instead of matching the line. truncateCode must always
// render the ellipsis with the line's own Code style, regardless of
// what's active right before the cut.
func TestTruncateCode_EllipsisStyledWithLineStyle(t *testing.T) {
	t.Parallel()

	ls := LineStyle{
		Code: lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000")).Background(lipgloss.Color("#00ff00")),
	}
	wantEllipsis := ls.Code.Render("…")

	tests := []struct {
		name    string
		content string
		width   int
	}{
		{
			// The cut lands right after a completely unstyled
			// (bare, no escape codes) run of spaces — the case
			// xchroma.Formatter produces for zero-style tokens.
			name:    "cut lands on a bare unstyled whitespace run",
			content: "\x1b[38;5;1mfunc\x1b[m         main() {",
			width:   6,
		},
		{
			// The cut lands mid-token, inside an actively colored
			// (non-whitespace) run.
			name:    "cut lands mid a styled token",
			content: "\x1b[38;5;2mhelloWorld\x1b[m",
			width:   4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateCode(tt.content, tt.width, ls)
			require.Contains(t, got, wantEllipsis,
				"the ellipsis must be rendered with the line's own Code style")
			require.LessOrEqual(t, ansi.StringWidth(got), tt.width)
		})
	}
}

// TestTruncateCode_NoTruncationLeavesContentUnchanged covers the common
// case: content that already fits must pass through untouched, with no
// ellipsis appended.
func TestTruncateCode_NoTruncationLeavesContentUnchanged(t *testing.T) {
	t.Parallel()

	ls := LineStyle{Code: lipgloss.NewStyle()}
	content := "\x1b[38;5;1mshort\x1b[m"
	require.Equal(t, content, truncateCode(content, 20, ls))
}

// TestTruncateCode_ZeroWidthDropsEllipsis mirrors ansi.Truncate's own
// behavior: when there isn't even room for the ellipsis itself, nothing
// is emitted rather than overflowing by one cell to fit it anyway.
func TestTruncateCode_ZeroWidthDropsEllipsis(t *testing.T) {
	t.Parallel()

	ls := LineStyle{Code: lipgloss.NewStyle()}
	require.Equal(t, "", truncateCode("hello", 0, ls))
}
