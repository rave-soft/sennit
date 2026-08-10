package common

import (
	"strings"

	"github.com/rave-soft/braid/internal/ui/styles"
)

// ScrollbarThumbBounds returns the thumb's start line and size (in track
// cells) for a scrollbar representing contentSize lines of content in a
// viewportSize-line viewport scrolled to offset, within a track of the
// given height. ok is false when no thumb is needed (content fits, or
// height is non-positive) — callers should not draw or hit-test a
// scrollbar in that case.
func ScrollbarThumbBounds(height, contentSize, viewportSize, offset int) (start, size int, ok bool) {
	if height <= 0 || contentSize <= viewportSize {
		return 0, 0, false
	}

	// Calculate thumb size (minimum 1 character).
	thumbSize := max(1, height*viewportSize/contentSize)

	// Calculate thumb position.
	maxOffset := contentSize - viewportSize
	if maxOffset <= 0 {
		return 0, 0, false
	}

	// Calculate where the thumb starts.
	trackSpace := height - thumbSize
	thumbPos := 0
	if trackSpace > 0 && maxOffset > 0 {
		thumbPos = min(trackSpace, offset*trackSpace/maxOffset)
	}

	return thumbPos, thumbSize, true
}

// ScrollbarOffsetForThumbStart maps a desired thumb start line (0-based,
// clamped to the track) back to a content offset, inverting the mapping
// ScrollbarThumbBounds uses. Returns 0 if there's no room to drag
// (maxOffset or track space <= 0).
func ScrollbarOffsetForThumbStart(thumbStart, thumbSize, height, contentSize, viewportSize int) int {
	maxOffset := contentSize - viewportSize
	trackSpace := height - thumbSize
	if maxOffset <= 0 || trackSpace <= 0 {
		return 0
	}

	thumbStart = max(0, min(trackSpace, thumbStart))
	return thumbStart * maxOffset / trackSpace
}

// Scrollbar renders a vertical scrollbar based on content and viewport size.
// Returns an empty string if content fits within viewport (no scrolling needed).
func Scrollbar(s *styles.Styles, height, contentSize, viewportSize, offset int) string {
	thumbPos, thumbSize, ok := ScrollbarThumbBounds(height, contentSize, viewportSize, offset)
	if !ok {
		return ""
	}

	// Build the scrollbar.
	var sb strings.Builder
	for i := range height {
		if i > 0 {
			sb.WriteString("\n")
		}
		if i >= thumbPos && i < thumbPos+thumbSize {
			sb.WriteString(s.Dialog.ScrollbarThumb.Render(styles.ScrollbarThumb))
		} else {
			sb.WriteString(s.Dialog.ScrollbarTrack.Render(styles.ScrollbarTrack))
		}
	}

	return sb.String()
}

// ScrollbarStyled renders a vertical scrollbar like Scrollbar, but draws the
// thumb with a hover-highlighted style when hovered is true. Used by the
// chat's draggable scrollbar to show hover/drag feedback.
func ScrollbarStyled(s *styles.Styles, height, contentSize, viewportSize, offset int, hovered bool) string {
	thumbPos, thumbSize, ok := ScrollbarThumbBounds(height, contentSize, viewportSize, offset)
	if !ok {
		return ""
	}

	thumbStyle := s.Dialog.ScrollbarThumb
	if hovered {
		thumbStyle = s.Dialog.ScrollbarThumbHover
	}

	var sb strings.Builder
	for i := range height {
		if i > 0 {
			sb.WriteString("\n")
		}
		if i >= thumbPos && i < thumbPos+thumbSize {
			sb.WriteString(thumbStyle.Render(styles.ScrollbarThumb))
		} else {
			sb.WriteString(s.Dialog.ScrollbarTrack.Render(styles.ScrollbarTrack))
		}
	}

	return sb.String()
}
