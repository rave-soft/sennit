package dialog

import (
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
)

// Base provides the width bookkeeping shared by dialogs that render as a
// single centered box: compute the dialog width from the available area
// (clamped to a max), and expose the content-area width (total minus the
// View frame) that children must size against per AGENTS.md's dialog
// rendering rules.
type Base struct {
	com      *common.Common
	maxWidth int
	width    int
}

// NewBase creates a Base whose dialog width is clamped to maxWidth.
func NewBase(com *common.Common, maxWidth int) Base {
	return Base{com: com, maxWidth: maxWidth}
}

// Resize recomputes Width()/InnerWidth() for the given area. Call this
// first in Draw, before sizing any child content.
func (b *Base) Resize(area uv.Rectangle) {
	t := b.com.Styles
	b.width = max(0, min(b.maxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
}

// Width returns the dialog's total box width, as of the last Resize.
func (b *Base) Width() int {
	return b.width
}

// InnerWidth returns the dialog's content area: Width() minus the View
// frame (border/padding). Size content to this, never to Width(), per
// AGENTS.md.
func (b *Base) InnerWidth() int {
	t := b.com.Styles
	return b.width - t.Dialog.View.GetHorizontalFrameSize()
}

// Frame wraps content in the dialog's View style at the current Width().
func (b *Base) Frame(content string) string {
	t := b.com.Styles
	return t.Dialog.View.Width(b.width).Render(content)
}
