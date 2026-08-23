package image

import (
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// solidImage returns an opaque, uniformly-colored w x h image, useful for
// tests that need to tell "colored" cells apart from letterboxed margin.
func solidImage(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	return img
}

// isBlankRow reports whether a rendered row carries no background-color
// escape sequence (i.e. it's unpainted letterbox margin).
func isBlankRow(row string) bool {
	return !strings.Contains(row, "\x1b[48;2;")
}

// TestPaint_LetterboxesInsteadOfStretching is a regression test: paint
// used to scale straight onto the full columns x rows canvas regardless
// of the source image's own aspect ratio, stretching a
// non-box-shaped image to fill the box. A wide (2:1) image in a square
// cell grid must letterbox — leave the top/bottom rows blank — rather
// than being stretched to fill every row.
func TestPaint_LetterboxesInsteadOfStretching(t *testing.T) {
	t.Parallel()

	// 200x100 pixels (2:1) with 10x10 square cells fits into a 20x10
	// cell rectangle. Requesting a 20x20 box must letterbox the extra
	// 10 rows rather than stretch the image to fill them.
	img := solidImage(200, 100, color.RGBA{R: 255, A: 255})

	out := paint(img, 20, 20, CellSize{Width: 10, Height: 10})
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 20)

	require.True(t, isBlankRow(lines[0]), "top margin row must be letterboxed, not painted")
	require.True(t, isBlankRow(lines[19]), "bottom margin row must be letterboxed, not painted")
	require.False(t, isBlankRow(lines[10]), "a row within the fitted image must be painted")
}

// TestPaint_AccountsForCellAspectRatio is a regression test for the
// second half of the same bug: even without letterboxing, blocks mode
// mapped one image pixel to one character cell, ignoring that a
// terminal cell is not square. A perfectly square source image, with
// cells twice as tall as they are wide, must render as roughly twice as
// many columns as rows — not a square block of cells, which would look
// visually stretched vertically on a real terminal.
func TestPaint_AccountsForCellAspectRatio(t *testing.T) {
	t.Parallel()

	img := solidImage(100, 100, color.RGBA{G: 255, A: 255})

	out := paint(img, 40, 40, CellSize{Width: 5, Height: 10})
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 40)

	paintedRows := 0
	var paintedCols int
	for _, line := range lines {
		n := strings.Count(line, "\x1b[48;2;")
		if n > 0 {
			paintedRows++
			paintedCols = n
		}
	}

	require.Equal(t, 10, paintedRows, "100px / 10px-tall cells = 10 rows")
	require.Equal(t, 20, paintedCols, "100px / 5px-wide cells = 20 columns")
	require.Equal(t, 2*paintedRows, paintedCols,
		"a square image with cells twice as tall as wide must render twice as many columns as rows")
}

// TestPaint_FillsFullBoxWhenAspectMatches is the negative case: when the
// source image's aspect (after accounting for cell shape) already
// matches the requested box exactly, there is no letterboxing and every
// cell is painted.
func TestPaint_FillsFullBoxWhenAspectMatches(t *testing.T) {
	t.Parallel()

	img := solidImage(100, 100, color.RGBA{B: 255, A: 255})

	out := paint(img, 10, 10, CellSize{Width: 10, Height: 10})
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 10)
	for i, line := range lines {
		require.Falsef(t, isBlankRow(line), "row %d should be fully painted when the aspect ratio already matches", i)
	}
}
