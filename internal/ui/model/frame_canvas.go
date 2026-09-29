package model

import uv "github.com/charmbracelet/ultraviolet"

// frameCanvas is the screen buffer a View draws each frame into, kept
// between frames. A fresh uv.NewScreenBuffer per frame allocated a full
// terminal of cells every time; while an agent streams, that was over
// half of everything the process allocated (2.8GB a minute on a large
// terminal), all of it garbage one frame later.
type frameCanvas struct {
	buf uv.ScreenBuffer
}

// next returns a blank width x height buffer: the kept one cleared, or a
// new one when there is none yet or the size changed.
func (c *frameCanvas) next(width, height int) uv.ScreenBuffer {
	if c.buf.RenderBuffer == nil || c.buf.Width() != width || c.buf.Height() != height {
		c.buf = uv.NewScreenBuffer(width, height)
		return c.buf
	}
	c.buf.Buffer.Clear()
	return c.buf
}
