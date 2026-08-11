package common

import (
	"image/color"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// BlockBackground repaints content as a solid width-wide block on bg: the
// styled string is drawn onto a cell buffer and every cell that doesn't
// already set its own background (e.g. code blocks keep theirs) gets bg,
// including the padding to the right of each line. This is the only way to
// put a uniform background behind already-ANSI-styled text — wrapping the
// whole thing in a lipgloss background style breaks at the first inner
// reset sequence.
func BlockBackground(content string, width int, bg color.Color) string {
	if width <= 0 || content == "" || bg == nil {
		return content
	}
	height := lipgloss.Height(content)
	buf := uv.NewScreenBuffer(width, height)
	uv.NewStyledString(content).Draw(buf, uv.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			cell := buf.CellAt(x, y)
			if cell != nil && cell.Style.Bg == nil {
				cell.Style.Bg = bg
			}
		}
	}
	return buf.Render()
}
