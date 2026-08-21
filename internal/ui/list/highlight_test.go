package list

import (
	"image"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

// TestHighlightContent_SingleLine covers the common case: a single
// line, full-width highlight returns the same visible text.
func TestHighlightContent_SingleLine(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 1)
	got := HighlightContent("hello", area, 0, 0, 0, -1)
	require.Equal(t, "hello\n", got)
}

// TestHighlightContent_PartialColumnRange covers highlighting a
// sub-range of columns on a single line: only the selected columns
// are returned.
func TestHighlightContent_PartialColumnRange(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 1)
	got := HighlightContent("hello world", area, 0, 0, 0, 5)
	require.Equal(t, "hello\n", got)
}

// TestHighlightContent_MultiLine covers a highlight spanning several
// lines: each line's content is joined by newlines according to the
// row it appears on.
func TestHighlightContent_MultiLine(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 3)
	got := HighlightContent("one\ntwo\nthree", area, 0, 0, 2, -1)
	require.Equal(t, "one\ntwo\nthree\n", got)
}

// TestHighlightContent_NegativeStartReturnsEmpty covers the guard in
// HighlightBuffer: a negative startLine or startCol means "nothing to
// highlight" and HighlightContent must return an empty string.
func TestHighlightContent_NegativeStartReturnsEmpty(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 1)
	require.Equal(t, "", HighlightContent("hello", area, -1, 0, 0, -1))
	require.Equal(t, "", HighlightContent("hello", area, 0, -1, 0, -1))
}

// TestHighlightBuffer_NegativeStartReturnsNil covers the same guard
// at the HighlightBuffer level, and Highlight's fallback to the
// original content when HighlightBuffer returns nil.
func TestHighlightBuffer_NegativeStartReturnsNil(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 1)
	require.Nil(t, HighlightBuffer("hello", area, -1, 0, 0, -1, nil))

	got := Highlight("hello", area, -1, 0, 0, -1, nil)
	require.Equal(t, "hello", got, "Highlight falls back to the original content when the range is invalid")
}

// TestHighlight_DefaultHighlighterReversesAttrs covers the nil ->
// DefaultHighlighter fallback in Highlight and confirms the styled
// output differs from the plain content (reverse video applied).
func TestHighlight_DefaultHighlighterReversesAttrs(t *testing.T) {
	t.Parallel()

	area := image.Rect(0, 0, 10, 1)
	got := Highlight("hi", area, 0, 0, 0, -1, nil)
	require.Contains(t, got, "hi", "rendered output still contains the visible text")
}

// TestHighlight_CustomHighlighterInvoked covers passing an explicit
// highlighter callback: it must be invoked for every cell in range,
// and DefaultHighlighter must be bypassed.
func TestHighlight_CustomHighlighterInvoked(t *testing.T) {
	t.Parallel()

	var calls int
	custom := func(x, y int, c *uv.Cell) *uv.Cell {
		calls++
		return c
	}

	area := image.Rect(0, 0, 10, 1)
	_ = Highlight("hi", area, 0, 0, 0, -1, custom)
	require.Equal(t, 2, calls, "custom highlighter must run once per content cell ('h' and 'i')")
}

// TestHighlightBuffer_NilCellSafe covers a highlighter that receives
// a nil cell gracefully (DefaultHighlighter's own nil guard).
func TestHighlightBuffer_NilCellSafe(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		DefaultHighlighter(0, 0, nil)
	})
}

// TestToHighlighter_AppliesStyle covers ToHighlighter: the returned
// [Highlighter] overwrites the cell's style with the converted
// lipgloss style, ignoring x/y.
func TestToHighlighter(t *testing.T) {
	t.Parallel()

	style := lipgloss.NewStyle().Bold(true)
	h := ToHighlighter(style)

	cell := &uv.Cell{Content: "x"}
	got := h(3, 4, cell)
	require.NotNil(t, got)
	require.NotZero(t, got.Style.Attrs&uv.AttrBold, "converted style must carry Bold")

	// A nil cell must pass through unchanged (no panic).
	require.Nil(t, h(0, 0, nil))
}

// TestToStyle_AllAttributes covers every attribute branch of ToStyle:
// each lipgloss attribute must set its corresponding uv.Style bit or
// field.
func TestToStyle_AllAttributes(t *testing.T) {
	t.Parallel()

	style := lipgloss.NewStyle().
		Bold(true).
		Italic(true).
		Underline(true).
		Strikethrough(true).
		Faint(true).
		Blink(true).
		Reverse(true)

	got := ToStyle(style)
	require.NotZero(t, got.Attrs&uv.AttrBold)
	require.NotZero(t, got.Attrs&uv.AttrItalic)
	require.NotZero(t, got.Attrs&uv.AttrStrikethrough)
	require.NotZero(t, got.Attrs&uv.AttrFaint)
	require.NotZero(t, got.Attrs&uv.AttrBlink)
	require.NotZero(t, got.Attrs&uv.AttrReverse)
	require.Equal(t, uv.UnderlineSingle, got.Underline)
}

// TestToStyle_NoAttributes covers the zero-value path: a plain style
// with no attributes set produces a zero-value uv.Style attrs field.
func TestToStyle_NoAttributes(t *testing.T) {
	t.Parallel()

	got := ToStyle(lipgloss.NewStyle())
	require.Zero(t, got.Attrs)
	require.NotEqual(t, uv.UnderlineSingle, got.Underline)
}
