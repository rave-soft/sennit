package dialog

import (
	"image"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// lineViewport implements the scroll/blit/scrollbar sequence shared by
// every question component that renders its content into a flat buffer
// of pre-styled rows and displays a scrollable window onto it: clamp
// the scroll offset to the buffer bounds, blit the visible rows, and
// draw a scrollbar when the buffer overflows the viewport. Building
// the row buffer itself stays with each caller since row types differ
// (question_confirm's line, question_freetext's ftLine,
// question_choice_base's contentLine) and so does any cursor-follow
// or snap-to-selection clamping on top of the plain bounds clamp.
type lineViewport struct {
	// Offset is the scroll offset in rows. Callers own the backing
	// field (e.g. c.scrollOffset) and round-trip it through this
	// struct for the duration of one Draw call.
	Offset int
}

// Clamp pins Offset to [0, max(0, total-viewport)] and reports
// whether the buffer overflows the viewport.
func (lv *lineViewport) Clamp(total, viewport int) (overflow bool) {
	limit := max(0, total-viewport)
	lv.Offset = min(max(0, lv.Offset), limit)
	return viewport > 0 && total > viewport
}

// Blit calls draw once for every visible row in [Offset,
// Offset+viewport), in buffer order, with the row's buffer index and
// its 0-based row within the viewport. draw is never called for a row
// outside that window, so callers that key per-row hit-test state (a
// button compositor, a hardware cursor) off "did draw run for my row
// this frame" get correct invalidation when that row scrolls out of
// view, without tracking visibility separately.
func (lv *lineViewport) Blit(total, viewport int, draw func(idx, screenRow int)) {
	for screenRow := range viewport {
		idx := lv.Offset + screenRow
		if idx >= total {
			return
		}
		draw(idx, screenRow)
	}
}

// DrawScrollbar paints a vertical scrollbar along the rightmost column
// of area when the buffer overflows the viewport. No-op otherwise.
func (lv *lineViewport) DrawScrollbar(scr uv.Screen, sty *styles.Styles, area uv.Rectangle, total, viewport int) {
	if viewport <= 0 || total <= viewport {
		return
	}
	sb := common.Scrollbar(sty, viewport, total, viewport, lv.Offset)
	if sb == "" {
		return
	}
	x := area.Max.X - 1
	uv.NewStyledString(sb).Draw(scr, image.Rect(x, area.Min.Y, x+1, area.Min.Y+viewport))
}
