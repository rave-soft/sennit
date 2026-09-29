package model

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"
)

// Not parallel: testing.AllocsPerRun refuses to run in a parallel test.
func TestFrameCanvasReusesAClearedBuffer(t *testing.T) {
	var c frameCanvas
	first := c.next(20, 3)
	uv.NewStyledString("leftover").Draw(first, first.Bounds())
	require.Equal(t, "l", first.CellAt(0, 0).Content)

	second := c.next(20, 3)
	require.Same(t, first.RenderBuffer, second.RenderBuffer, "same size must reuse the buffer")
	require.Equal(t, uv.EmptyCell, *second.CellAt(0, 0), "a reused buffer must come back blank")

	allocs := testing.AllocsPerRun(10, func() { c.next(20, 3) })
	require.Zero(t, allocs, "reusing the buffer must not allocate")

	resized := c.next(30, 4)
	require.NotSame(t, first.RenderBuffer, resized.RenderBuffer)
	require.Equal(t, 30, resized.Width())
	require.Equal(t, 4, resized.Height())
}
