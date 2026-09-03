package list

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestList_ScrollBy_Empty covers the empty-list guard: ScrollBy must
// be a no-op and never panic when there are no items.
func TestList_ScrollBy_Empty(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	require.NotPanics(t, func() { l.ScrollBy(10) })
	require.NotPanics(t, func() { l.ScrollBy(-10) })
	require.Equal(t, 0, l.Offset())
}

// TestList_ScrollBy_ZeroIsNoop covers the lines == 0 guard.
func TestList_ScrollBy_ZeroIsNoop(t *testing.T) {
	t.Parallel()

	items := []Item{newMultiLineItem("a", 20)}
	l := NewList(items...)
	l.SetSize(20, 5)
	l.ScrollBy(3)
	before := l.Offset()
	l.ScrollBy(0)
	require.Equal(t, before, l.Offset(), "zero delta must not move the offset")
}

// TestList_ScrollBy_PositiveMovesDown covers a plain downward scroll
// that stays within a single item's bounds.
func TestList_ScrollBy_PositiveMovesDown(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 20))
	l.SetSize(20, 5)
	require.Equal(t, 0, l.Offset())

	l.ScrollBy(3)
	require.Equal(t, 3, l.Offset())
}

// TestList_ScrollBy_NegativeMovesUp covers upward scroll from a
// non-zero offset.
func TestList_ScrollBy_NegativeMovesUp(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 20))
	l.SetSize(20, 5)
	l.ScrollBy(10)
	require.Equal(t, 10, l.Offset())

	l.ScrollBy(-4)
	require.Equal(t, 6, l.Offset())
}

// TestList_ScrollBy_CrossesItemBoundaryDown covers scrolling down far
// enough to move offsetIdx to a later item, including gap accounting.
func TestList_ScrollBy_CrossesItemBoundaryDown(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 4)
	b := newMultiLineItem("b", 10)
	l := NewList(a, b)
	l.SetGap(2)
	l.SetSize(20, 3)

	// Scroll past all of a's 4 lines plus the 2-line gap: lands 1 line
	// into b.
	l.ScrollBy(4 + 2 + 1)
	require.Equal(t, 1, l.offsetIdx)
	require.Equal(t, 1, l.offsetLine)
}

// TestList_ScrollBy_CrossesItemBoundaryUp is the upward mirror.
func TestList_ScrollBy_CrossesItemBoundaryUp(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 4)
	b := newMultiLineItem("b", 10)
	l := NewList(a, b)
	l.SetGap(2)
	l.SetSize(20, 3)

	l.ScrollToIndex(1)
	l.offsetLine = 1 // one line into b

	l.ScrollBy(-2)
	// Scrolling up re-adds the previous item's height *and* its gap, so
	// offsetLine may land inside the gap region rather than on a content
	// line: a occupies lines 0-3 and the 2-line gap occupies 4-5, so two
	// lines up from b's line 0 is the gap's last line, (0, 5). This is a
	// valid state, not an overshoot -- Offset() reports 5, exactly 2 less
	// than the 7 that (1, 1) reports, and ScrollBy(+2) returns to (1, 1).
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 5, l.offsetLine, "two lines up from b's first line is the gap's last line")
	require.Equal(t, 5, l.Offset(), "Offset() stays consistent with the item/line pair")

	l.ScrollBy(2)
	require.Equal(t, 1, l.offsetIdx, "scrolling back down returns to the starting position")
	require.Equal(t, 1, l.offsetLine)
}

// TestList_ScrollBy_ClampsAtBottom covers scrolling past the last
// line: the offset clamps to the bottom instead of overshooting, and
// a second downward scroll from the bottom is a no-op.
// TestList_ScrollBy_ClampsWithinLastItem covers the clamp-to-bottom
// arm of ScrollBy that TestList_ScrollBy_ClampsAtBottom cannot reach.
// With a single item, overscrolling exhausts the item list and returns
// early through ScrollToBottom, so the trailing lastOffsetItem() clamp
// is never executed. Two items and an overscroll that lands *inside*
// the last item exercise it: the raw walk would leave offsetLine at 7,
// one line past the true bottom of 6.
func TestList_ScrollBy_ClampsWithinLastItem(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 4), newMultiLineItem("b", 10))
	l.SetSize(20, 4)

	wantIdx, wantLine := l.lastOffsetItem()
	require.Equal(t, 1, wantIdx)
	require.Equal(t, 6, wantLine)

	l.ScrollBy(11)
	require.Equal(t, wantIdx, l.offsetIdx, "must not scroll past the last item")
	require.Equal(t, wantLine, l.offsetLine, "must clamp to the bottom line, not overshoot to 7")
	require.True(t, l.AtBottom())
}

func TestList_ScrollBy_ClampsAtBottom(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 10))
	l.SetSize(20, 4)

	l.ScrollBy(1000)
	require.True(t, l.AtBottom())
	offsetAtBottom := l.Offset()

	l.ScrollBy(50)
	require.Equal(t, offsetAtBottom, l.Offset(), "scrolling further down from the bottom is a no-op")
}

// TestList_ScrollBy_ClampsAtTop covers scrolling past the first line:
// the offset clamps to zero rather than going negative.
func TestList_ScrollBy_ClampsAtTop(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 10))
	l.SetSize(20, 4)
	l.ScrollBy(3)

	l.ScrollBy(-1000)
	require.Equal(t, 0, l.Offset())
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollBy_ReverseInvertsDirection covers the reverse-mode
// sign flip: a positive delta in reverse mode scrolls up instead of
// down.
func TestList_ScrollBy_ReverseInvertsDirection(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 20))
	l.SetSize(20, 5)
	l.SetReverse(true)

	// Start somewhere in the middle so both directions have room.
	l.offsetIdx = 0
	l.offsetLine = 10

	l.ScrollBy(3) // reverse: treated as -3, so offset should decrease
	require.Equal(t, 7, l.Offset())
}

// TestList_ScrollToIndex_ClampsNegative covers a negative target
// index clamping to zero.
func TestList_ScrollToIndex_ClampsNegative(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 3), newMultiLineItem("b", 3), newMultiLineItem("c", 3))
	l.SetSize(20, 2)

	l.ScrollToIndex(-5)
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollToIndex_ClampsAboveRange covers an out-of-range high
// target index clamping to the last item.
func TestList_ScrollToIndex_ClampsAboveRange(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 3), newMultiLineItem("b", 3), newMultiLineItem("c", 3))
	l.SetSize(20, 2)

	l.ScrollToIndex(99)
	require.Equal(t, 2, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollToIndex_AboveCurrentViewport covers jumping to an
// item that sits above the current viewport: the offset must move to
// that item's top.
func TestList_ScrollToIndex_AboveCurrentViewport(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 3)
	}
	l := NewList(items...)
	l.SetSize(20, 3)
	l.ScrollToIndex(4)
	require.Equal(t, 4, l.offsetIdx)

	l.ScrollToIndex(0)
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollToIndex_BelowCurrentViewport covers jumping forward
// to an item below the current viewport.
func TestList_ScrollToIndex_BelowCurrentViewport(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 3)
	}
	l := NewList(items...)
	l.SetSize(20, 3)

	l.ScrollToIndex(3)
	require.Equal(t, 3, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollToIndex_AlreadyInsideViewport covers the case where
// the target index is already the current offset: it's a no-op in
// terms of resulting position (offsetLine still resets to 0).
func TestList_ScrollToIndex_AlreadyInsideViewport(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 10))
	l.SetSize(20, 3)
	l.offsetLine = 4

	l.ScrollToIndex(0)
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine, "ScrollToIndex always resets offsetLine, even for the already-current item")
}

// TestList_ScrollToIndex_EmptyList covers calling ScrollToIndex on an
// empty list: it must not panic, and a subsequent Render must still
// return the empty string.
func TestList_ScrollToIndex_EmptyList(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	require.NotPanics(t, func() { l.ScrollToIndex(0) })
	require.Equal(t, "", l.Render())
}

// TestList_AtBottom_EmptyListIsTrue covers AtBottom's empty-list
// short-circuit.
func TestList_AtBottom_EmptyListIsTrue(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	require.True(t, l.AtBottom())
}

// TestList_AtBottom_SingleItemFitsViewport covers a single short item
// that fits entirely inside the viewport: always at bottom regardless
// of offset.
func TestList_AtBottom_SingleItemFitsViewport(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 3))
	l.SetSize(20, 10)
	require.True(t, l.AtBottom())
}

// TestList_AtBottom_BeforeAndAfterAppend covers the workflow this
// state machine exists for: a list scrolled to the bottom, followed
// by an append, must report AtBottom() == false until the caller
// explicitly re-follows (ScrollToBottom).
func TestList_AtBottom_BeforeAndAfterAppend(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 10))
	l.SetSize(20, 5)

	l.ScrollToBottom()
	require.True(t, l.AtBottom(), "explicitly scrolled to bottom")

	l.AppendItems(newMultiLineItem("b", 10))
	require.False(t, l.AtBottom(), "appending new content below the fold must break AtBottom")

	l.ScrollToBottom()
	require.True(t, l.AtBottom(), "re-following restores AtBottom")
}

// TestList_AtBottom_TallFirstVisibleItemScrolledToBottom covers the
// early-exit bail inside AtBottom's loop, which used to compare the
// running totalHeight against l.height without subtracting
// l.offsetLine (unlike the loop's final return, which does). When
// the first visible item is taller than the viewport and partially
// scrolled off the top, that early check saw the item's FULL height
// exceed l.height on the very next iteration and bailed out
// reporting "not at bottom" even though the true remaining content
// (item height minus offsetLine, plus any items after it) fits
// exactly inside the viewport.
func TestList_AtBottom_TallFirstVisibleItemScrolledToBottom(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 20)
	b := newMultiLineItem("b", 1)
	l := NewList(a, b)
	l.SetSize(20, 5)

	// Scroll so a's last 4 lines plus all of b's 1 line exactly fill
	// the 5-line viewport: offsetIdx stays on a (idx 0), which is far
	// taller than the viewport, while the list is genuinely at the
	// bottom.
	l.offsetIdx = 0
	l.offsetLine = 16

	require.True(t, l.AtBottom(),
		"a tall, partially-scrolled first item must not make AtBottom bail out early")
}

// TestList_ScrollToBottom_EmptyListNoop covers the empty-list guard
// in ScrollToBottom.
func TestList_ScrollToBottom_EmptyListNoop(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	require.NotPanics(t, func() { l.ScrollToBottom() })
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_ScrollToBottom_SingleItemLargerThanViewport covers the
// lastOffsetItem calculation directly: a single item taller than the
// viewport must land with offsetLine equal to the overflow amount.
func TestList_ScrollToBottom_SingleItemLargerThanViewport(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 10))
	l.SetSize(20, 4)

	l.ScrollToBottom()
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 6, l.offsetLine, "10-line item in a 4-line viewport leaves 6 lines above the fold")
}

// TestList_VisibleItemIndices_Empty covers the empty-list guard.
func TestList_VisibleItemIndices_Empty(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	start, end := l.VisibleItemIndices()
	require.Equal(t, 0, start)
	require.Equal(t, 0, end)
}

// TestList_VisibleItemIndices_SingleItemViewport covers a viewport
// that only ever shows one item.
func TestList_VisibleItemIndices_SingleItemViewport(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 10)
	}
	l := NewList(items...)
	l.SetSize(20, 3)

	start, end := l.VisibleItemIndices()
	require.Equal(t, 0, start)
	require.Equal(t, 0, end)
}

// TestList_VisibleItemIndices_SpansMultipleItems covers a viewport
// tall enough to span several short items.
func TestList_VisibleItemIndices_SpansMultipleItems(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 5) // fits 2.5 items

	start, end := l.VisibleItemIndices()
	require.Equal(t, 0, start)
	require.Equal(t, 2, end)
}

// TestList_SelectedItemInView_NoSelection covers the -1 guard.
func TestList_SelectedItemInView_NoSelection(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 3))
	l.SetSize(20, 5)
	require.False(t, l.SelectedItemInView())
}

// TestList_SelectedItemInView_TrueWhenVisible covers the common case
// where the selected index falls inside the visible range.
func TestList_SelectedItemInView_TrueWhenVisible(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 5)

	l.SetSelected(1)
	require.True(t, l.SelectedItemInView())
}

// TestList_SelectedItemInView_FalseWhenScrolledAway covers the
// selected item being pushed out of view by scrolling, without a
// ScrollToSelected call to bring it back.
func TestList_SelectedItemInView_FalseWhenScrolledAway(t *testing.T) {
	t.Parallel()

	items := make([]Item, 10)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 4)

	l.SetSelected(0)
	require.True(t, l.SelectedItemInView())

	l.ScrollToIndex(9)
	require.False(t, l.SelectedItemInView(), "selection at index 0 is no longer visible after scrolling to the end")
}

// TestList_ScrollToSelected_NoSelectionNoop covers the -1/out-of-
// range guard.
func TestList_ScrollToSelected_NoSelectionNoop(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 3))
	l.SetSize(20, 5)
	require.NotPanics(t, func() { l.ScrollToSelected() })
	require.Equal(t, 0, l.offsetIdx)
}

// TestList_ScrollToSelected_AboveViewportDragsUp covers selecting an
// item above the visible range and calling ScrollToSelected: the
// viewport must move so the selected item lands at the top.
func TestList_ScrollToSelected_AboveViewportDragsUp(t *testing.T) {
	t.Parallel()

	items := make([]Item, 10)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 4)
	l.ScrollToIndex(8) // viewport near the bottom

	l.SetSelected(0)
	l.ScrollToSelected()
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
	require.True(t, l.SelectedItemInView())
}

// TestList_ScrollToSelected_BelowViewportDragsDown covers selecting
// an item below the visible range: the viewport must move so the
// selected item lands at the bottom.
func TestList_ScrollToSelected_BelowViewportDragsDown(t *testing.T) {
	t.Parallel()

	items := make([]Item, 10)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 4) // shows 2 items at a time

	l.SetSelected(9)
	l.ScrollToSelected()
	require.True(t, l.SelectedItemInView())
	_, end := l.VisibleItemIndices()
	require.Equal(t, 9, end, "selected item must be the last visible one")
}

// TestList_ScrollToSelected_AlreadyInViewNoop covers the case where
// the selected item is already visible: the offset must not change.
func TestList_ScrollToSelected_AlreadyInViewNoop(t *testing.T) {
	t.Parallel()

	items := make([]Item, 10)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 4)
	l.SetSelected(0)

	before := l.offsetIdx
	l.ScrollToSelected()
	require.Equal(t, before, l.offsetIdx)
}

// TestList_ScrollToSelected_AllItemsFitScrollsToTop covers the branch
// where every item fits in the viewport: the selection is necessarily
// already visible, so ScrollToSelected does nothing.
//
// Note: the `if totalHeight < l.height { l.ScrollToTop() }` tail at
// list.go:795 is unreachable. Reaching it requires the selection to be
// below the visible range, which means the content already exceeds the
// viewport, in which case the loop summing back from selectedIdx always
// reaches l.height and breaks first. Left in place, not tested.
func TestList_ScrollToSelected_AllItemsFitIsNoOp(t *testing.T) {
	t.Parallel()

	items := make([]Item, 3)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 100) // way taller than total content

	l.SetSelected(2)
	l.offsetIdx = 1
	l.ScrollToSelected()

	// ScrollToSelected only acts when the selection is outside the visible
	// range. When every item fits, nothing is ever outside it, so the call
	// is a no-op and the forced offset survives untouched.
	require.Equal(t, 1, l.offsetIdx, "selection already visible: no scrolling")
	require.Equal(t, 0, l.offsetLine)
	require.True(t, l.SelectedItemInView())
}

// TestList_Offset_AccumulatesPriorItemHeightsAndGaps covers Offset()
// directly: it must sum the heights (and gaps) of every item before
// offsetIdx, plus offsetLine.
func TestList_Offset_AccumulatesPriorItemHeightsAndGaps(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 3)
	b := newMultiLineItem("b", 4)
	c := newMultiLineItem("c", 5)
	l := NewList(a, b, c)
	l.SetGap(1)
	l.SetSize(20, 3)

	l.offsetIdx = 2
	l.offsetLine = 2

	// a's height (3) is a one-liner? No, height 3, gapAt only
	// collapses for two one-line neighbors, so the gap stays 1
	// between each pair: 3+1 (a + gap) + 4+1 (b + gap) + 2 (partial
	// c) = 11.
	require.Equal(t, 3+1+4+1+2, l.Offset())
}

// TestList_Offset_ZeroAtTop covers the trivial zero case.
func TestList_Offset_ZeroAtTop(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 5))
	l.SetSize(20, 3)
	require.Equal(t, 0, l.Offset())
}

// TestList_SetSelected_ValidIndex covers the in-range branch.
func TestList_SetSelected_ValidIndex(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1))
	l.SetSelected(1)
	require.Equal(t, 1, l.Selected())
}

// TestList_SetSelected_OutOfRangeResetsToNone covers both
// out-of-range directions: negative and >= len(items) both reset the
// selection to -1.
func TestList_SetSelected_OutOfRangeResetsToNone(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1))
	l.SetSelected(0)
	require.Equal(t, 0, l.Selected())

	l.SetSelected(-1)
	require.Equal(t, -1, l.Selected())

	l.SetSelected(1)
	l.SetSelected(5)
	require.Equal(t, -1, l.Selected())
}

// TestList_SetSelected_EmptyList covers calling SetSelected on an
// empty list: any index is out of range, so selection resets to -1.
func TestList_SetSelected_EmptyList(t *testing.T) {
	t.Parallel()

	l := NewList()
	require.NotPanics(t, func() { l.SetSelected(0) })
	require.Equal(t, -1, l.Selected())
}

// TestList_FocusBlurFocused covers the trivial focus-state trio.
func TestList_FocusBlurFocused(t *testing.T) {
	t.Parallel()

	l := NewList()
	require.False(t, l.Focused())
	l.Focus()
	require.True(t, l.Focused())
	l.Blur()
	require.False(t, l.Focused())
}

// TestList_WidthHeightLenGap covers the plain accessor quartet.
func TestList_WidthHeightLenGap(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1))
	l.SetSize(30, 12)
	l.SetGap(2)

	require.Equal(t, 30, l.Width())
	require.Equal(t, 12, l.Height())
	require.Equal(t, 2, l.Len())
	require.Equal(t, 2, l.Gap())
}

// TestList_RegisterRenderCallback covers a callback that mutates the
// rendered item: the list must apply it before rendering and the
// mutation must be visible in the output.
func TestList_RegisterRenderCallback(t *testing.T) {
	t.Parallel()

	a := newTrackedItem("a", "alpha", false)
	l := NewList(a)
	l.SetSize(20, 5)

	var seenIdx, seenSelected int
	var seenItem Item
	l.RegisterRenderCallback(func(idx, selectedIdx int, item Item) Item {
		seenIdx, seenSelected, seenItem = idx, selectedIdx, item
		return item
	})
	l.SetSelected(0)
	_ = l.Render()

	require.Equal(t, 0, seenIdx)
	require.Equal(t, 0, seenSelected)
	require.Equal(t, a, seenItem)
}

// TestList_RegisterRenderCallback_ReplacesItem covers a callback that
// returns a different item than the one passed in — the list must
// render the callback's replacement, not the original.
func TestList_RegisterRenderCallback_ReplacesItem(t *testing.T) {
	t.Parallel()

	a := newTrackedItem("a", "alpha", false)
	replacement := newTrackedItem("a", "REPLACED", false)
	l := NewList(a)
	l.SetSize(20, 5)

	l.RegisterRenderCallback(func(idx, selectedIdx int, item Item) Item {
		return replacement
	})

	out := l.Render()
	require.Contains(t, out, "REPLACED")
	require.Equal(t, 1, replacement.renderHits)
}

// TestList_Invalidate_DropsSingleEntry covers the exported Invalidate
// wrapper: it must force a re-render of only the targeted item.
func TestList_Invalidate_DropsSingleEntry(t *testing.T) {
	t.Parallel()

	a := newTrackedItem("a", "alpha", true)
	b := newTrackedItem("b", "bravo", true)
	l := NewList(a, b)
	l.SetSize(20, 5)
	_ = l.Render()
	require.Equal(t, 1, a.renderHits)
	require.Equal(t, 1, b.renderHits)

	l.Invalidate(a)
	_ = l.Render()
	require.Equal(t, 2, a.renderHits, "invalidated item must re-render")
	require.Equal(t, 1, b.renderHits, "untouched item must stay cached")
}

// invalidatableItem is a test double for InvalidateAll: it exposes an
// Invalidate() method (the interface List.InvalidateAll probes for)
// separately from the render-cache invalidation, so the test can
// confirm both effects independently.
type invalidatableItem struct {
	*trackedItem
	invalidated int
}

func (i *invalidatableItem) Invalidate() { i.invalidated++ }

// TestList_InvalidateAll_ClearsCacheAndCallsItemHook covers
// InvalidateAll: every item's own Invalidate() must be called, and
// the list-level cache must be dropped so the next render is fresh.
func TestList_InvalidateAll_ClearsCacheAndCallsItemHook(t *testing.T) {
	t.Parallel()

	a := &invalidatableItem{trackedItem: newTrackedItem("a", "alpha", true)}
	l := NewList(a)
	l.SetSize(20, 5)
	_ = l.Render()
	require.Equal(t, 1, a.renderHits)

	l.InvalidateAll()
	require.Equal(t, 1, a.invalidated, "item-level Invalidate hook must be called")

	_ = l.Render()
	require.Equal(t, 2, a.renderHits, "list-level cache must also be cleared")
}

// TestList_IsSelectedFirstLast covers the boundary predicates across
// an empty selection, first, middle, and last positions.
func TestList_IsSelectedFirstLast(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))

	l.SetSelected(0)
	require.True(t, l.IsSelectedFirst())
	require.False(t, l.IsSelectedLast())

	l.SetSelected(1)
	require.False(t, l.IsSelectedFirst())
	require.False(t, l.IsSelectedLast())

	l.SetSelected(2)
	require.False(t, l.IsSelectedFirst())
	require.True(t, l.IsSelectedLast())
}

// TestList_SelectPrevNext_Normal covers SelectPrev/SelectNext in
// normal (non-reverse) mode, including the boundary "no more room"
// case returning false.
func TestList_SelectPrevNext_Normal(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))
	l.SetSelected(1)

	require.True(t, l.SelectPrev())
	require.Equal(t, 0, l.Selected())
	require.False(t, l.SelectPrev(), "already at the first item")

	l.SetSelected(1)
	require.True(t, l.SelectNext())
	require.Equal(t, 2, l.Selected())
	require.False(t, l.SelectNext(), "already at the last item")
}

// TestList_SelectPrevNext_Reverse covers the direction flip under
// SetReverse(true): visual "up" (SelectPrev) now moves toward the
// highest index.
func TestList_SelectPrevNext_Reverse(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))
	l.SetReverse(true)
	l.SetSelected(1)

	require.True(t, l.SelectPrev())
	require.Equal(t, 2, l.Selected(), "reverse mode: visual up increases the index")

	l.SetSelected(1)
	require.True(t, l.SelectNext())
	require.Equal(t, 0, l.Selected(), "reverse mode: visual down decreases the index")
}

// TestList_SelectFirstLast_Empty covers the empty-list guard shared
// by SelectFirst/SelectLast.
func TestList_SelectFirstLast_Empty(t *testing.T) {
	t.Parallel()

	l := NewList()
	require.False(t, l.SelectFirst())
	require.False(t, l.SelectLast())
}

// TestList_SelectFirstLast covers the normal-mode selection jumps.
func TestList_SelectFirstLast(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))

	require.True(t, l.SelectFirst())
	require.Equal(t, 0, l.Selected())

	require.True(t, l.SelectLast())
	require.Equal(t, 2, l.Selected())
}

// TestList_WrapToStartEnd_Empty covers the empty-list guard.
func TestList_WrapToStartEnd_Empty(t *testing.T) {
	t.Parallel()

	l := NewList()
	require.False(t, l.WrapToStart())
	require.False(t, l.WrapToEnd())
}

// TestList_WrapToStartEnd_Normal covers wrap targets in normal mode.
func TestList_WrapToStartEnd_Normal(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))

	require.True(t, l.WrapToStart())
	require.Equal(t, 0, l.Selected())

	require.True(t, l.WrapToEnd())
	require.Equal(t, 2, l.Selected())
}

// TestList_WrapToStartEnd_Reverse covers wrap targets in reverse
// mode: start/end swap indices.
func TestList_WrapToStartEnd_Reverse(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1), newMultiLineItem("b", 1), newMultiLineItem("c", 1))
	l.SetReverse(true)

	require.True(t, l.WrapToStart())
	require.Equal(t, 2, l.Selected())

	require.True(t, l.WrapToEnd())
	require.Equal(t, 0, l.Selected())
}

// TestList_SelectedItem covers both the nil (no selection) and
// present cases.
func TestList_SelectedItem(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 1)
	l := NewList(a, b)

	require.Nil(t, l.SelectedItem(), "no selection yet")

	l.SetSelected(1)
	require.Equal(t, Item(b), l.SelectedItem())
}

// TestList_SelectFirstLastInView covers selecting the boundary of the
// currently visible range.
func TestList_SelectFirstLastInView(t *testing.T) {
	t.Parallel()

	items := make([]Item, 10)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 2)
	}
	l := NewList(items...)
	l.SetSize(20, 4) // shows indices 0..1
	l.ScrollToIndex(3)

	l.SelectFirstInView()
	require.Equal(t, 3, l.Selected())

	l.SelectLastInView()
	start, end := l.VisibleItemIndices()
	require.Equal(t, end, l.Selected())
	require.GreaterOrEqual(t, l.Selected(), start)
}

// TestList_ItemAt covers in-range and out-of-range lookups.
func TestList_ItemAt(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	l := NewList(a)

	require.Equal(t, Item(a), l.ItemAt(0))
	require.Nil(t, l.ItemAt(-1))
	require.Nil(t, l.ItemAt(1))
}

// TestList_ItemIndexAtPosition_OutOfBounds covers y coordinates
// outside the viewport height.
func TestList_ItemIndexAtPosition_OutOfBounds(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 5))
	l.SetSize(20, 5)

	idx, y := l.ItemIndexAtPosition(0, -1)
	require.Equal(t, -1, idx)
	require.Equal(t, -1, y)

	idx, y = l.ItemIndexAtPosition(0, 5)
	require.Equal(t, -1, idx)
	require.Equal(t, -1, y)
}

// TestList_ItemIndexAtPosition_LocatesItemAndOffset covers the
// success path across item boundaries and a gap.
func TestList_ItemIndexAtPosition_LocatesItemAndOffset(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 3)
	b := newMultiLineItem("b", 3)
	l := NewList(a, b)
	l.SetGap(2)
	l.SetSize(20, 10)

	idx, y := l.ItemIndexAtPosition(0, 0)
	require.Equal(t, 0, idx)
	require.Equal(t, 0, y)

	idx, y = l.ItemIndexAtPosition(0, 2)
	require.Equal(t, 0, idx)
	require.Equal(t, 2, y, "last line of item a")

	// Row 3 and 4 fall inside the 2-line gap: no item there.
	idx, _ = l.ItemIndexAtPosition(0, 3)
	require.Equal(t, -1, idx, "gap row must not resolve to an item")

	idx, y = l.ItemIndexAtPosition(0, 5) // 3 (a) + 2 (gap) = start of b
	require.Equal(t, 1, idx)
	require.Equal(t, 0, y)
}

// TestList_ItemIndexAtPosition_EmptyList covers the case where the
// list has no items at all: it must return -1, -1 without panicking.
func TestList_ItemIndexAtPosition_EmptyList(t *testing.T) {
	t.Parallel()

	l := NewList()
	l.SetSize(20, 5)
	idx, y := l.ItemIndexAtPosition(0, 0)
	require.Equal(t, -1, idx)
	require.Equal(t, -1, y)
}

// TestList_PrependItems_ShiftsOffsetAndSelection covers the index
// bookkeeping in PrependItems: existing offset and selection indices
// must shift by the number of prepended items so the same content
// stays in view/selected.
func TestList_PrependItems_ShiftsOffsetAndSelection(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 1)
	l := NewList(a, b)
	l.SetSize(20, 1)
	l.SetSelected(1) // b
	l.ScrollToIndex(1)

	l.PrependItems(newMultiLineItem("z", 1), newMultiLineItem("y", 1))
	require.Equal(t, 3, l.offsetIdx, "offset shifts by the 2 prepended items")
	require.Equal(t, 3, l.Selected(), "selection shifts by the 2 prepended items")
	require.Equal(t, Item(b), l.ItemAt(3))
}

// TestList_RemoveItem_OutOfRangeNoop covers RemoveItem's bounds
// guard.
func TestList_RemoveItem_OutOfRangeNoop(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1))
	require.NotPanics(t, func() { l.RemoveItem(-1) })
	require.NotPanics(t, func() { l.RemoveItem(5) })
	require.Equal(t, 1, l.Len())
}

// TestList_RemoveItem_ClearsSelectionWhenRemoved covers RemoveItem
// resetting selection to -1 when the removed index was selected, and
// decrementing it when a later item is removed.
func TestList_RemoveItem_ClearsSelectionWhenRemoved(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 1)
	c := newMultiLineItem("c", 1)
	l := NewList(a, b, c)

	l.SetSelected(1) // b
	l.RemoveItem(1)
	require.Equal(t, -1, l.Selected(), "removing the selected item clears the selection")

	l.SetSelected(1) // now b's old slot holds c
	l.RemoveItem(0)  // remove a, before the selected index
	require.Equal(t, 0, l.Selected(), "selection index shifts down when an earlier item is removed")
}

// TestList_RemoveItem_AdjustsOffsetPastEnd covers RemoveItem trimming
// the offset back into range when the removed item was the last one
// and the offset pointed at it.
func TestList_RemoveItem_AdjustsOffsetPastEnd(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 1)
	l := NewList(a, b)
	l.SetSize(20, 1)
	l.ScrollToIndex(1) // offset at the last item

	l.RemoveItem(1)
	require.Equal(t, 0, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_RemoveItem_ResetsOffsetLineWhenItemAtOffsetIsRemoved covers
// removing the item the viewport is scrolled into, when items remain
// after it. offsetLine is a line count measured against the specific
// item that used to sit at offsetIdx; once that item is gone, a
// different item takes its slot and the stale offsetLine can point
// past that item's height, dropping its content from Render entirely.
func TestList_RemoveItem_ResetsOffsetLineWhenItemAtOffsetIsRemoved(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 3)
	c := newMultiLineItem("c", 1)
	l := NewList(a, b, c)
	l.SetSize(20, 5)

	// Scroll one line into b, so offsetIdx points at b with a nonzero
	// offsetLine.
	l.offsetIdx = 1
	l.offsetLine = 1

	l.RemoveItem(1) // remove b; c now occupies index 1
	require.Equal(t, 1, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine, "offsetLine must not survive the item it was measured against")

	require.Contains(t, l.Render(), "c:0", "c must still render instead of the viewport going blank")
}

// TestList_ShrinkingOffsetItem_ClampsOffsetLine covers the second
// path into the same failure RemoveItem's fix closed: the item at
// offsetIdx keeps its identity but gets shorter, e.g. a streaming
// update that bumps its version and shrinks its rendered output. A
// stale offsetLine left pointing past the item's new end sends
// Render down the "offset starts inside the gap" branch and drops
// the item entirely, same as the removal case.
func TestList_ShrinkingOffsetItem_ClampsOffsetLine(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 1)
	b := newMultiLineItem("b", 10)
	l := NewList(a, b)
	l.SetSize(20, 5)

	// Scroll deep into b, well past where b will shrink to.
	l.offsetIdx = 1
	l.offsetLine = 8

	// Simulate a streaming update that shrinks b's rendered output
	// without changing its identity or index.
	b.height = 3
	b.Bump()

	require.Contains(t, l.Render(), "b:2", "b's new last line must still render instead of the viewport going blank")
	require.Equal(t, 2, l.offsetLine, "offsetLine must clamp to b's new last line")
}

// TestList_SetItems_ClampsSelectionAndOffset covers SetItems
// shrinking the item set below the current selection/offset indices:
// both must clamp into range rather than leaving stale out-of-bounds
// values.
func TestList_SetItems_ClampsSelectionAndOffset(t *testing.T) {
	t.Parallel()

	items := make([]Item, 5)
	for i := range items {
		items[i] = newMultiLineItem(string(rune('a'+i)), 1)
	}
	l := NewList(items...)
	l.SetSelected(4)
	l.ScrollToIndex(4)

	l.SetItems(newMultiLineItem("x", 1), newMultiLineItem("y", 1))
	require.Equal(t, 1, l.Selected())
	require.Equal(t, 1, l.offsetIdx)
	require.Equal(t, 0, l.offsetLine)
}

// TestList_SetItems_EmptySlice covers SetItems([]) — clearing the
// list entirely must not panic and must reset selection/offset.
func TestList_SetItems_EmptySlice(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1))
	l.SetSelected(0)

	require.NotPanics(t, func() { l.SetItems() })
	require.Equal(t, 0, l.Len())
	require.Equal(t, -1, l.Selected(), "clamping selection against an empty list yields -1")
}

// TestList_SetItems_EmptyThenRepopulated covers a regression where
// SetItems([]) clamped offsetIdx to -1 (via a bare min(offsetIdx,
// len-1) with len==0) and left it there permanently: min(-1, n-1) is
// always -1 for any later non-empty item set, so the list rendered as
// empty forever after being cleared once. offsetIdx must clamp to 0,
// not -1, when the list is emptied.
func TestList_SetItems_EmptyThenRepopulated(t *testing.T) {
	t.Parallel()

	l := NewList(newMultiLineItem("a", 1))
	l.SetSize(20, 5)

	l.SetItems()
	require.Equal(t, 0, l.Len())

	l.SetItems(newMultiLineItem("x", 1), newMultiLineItem("y", 1))
	require.Equal(t, 0, l.offsetIdx, "offsetIdx must not stay stuck at -1 after repopulating")
	require.NotEmpty(t, l.Render(), "a repopulated list must render its items, not stay blank")
}

// TestList_ScrollBy_StopsWithinGapInsteadOfJumping covers scrolling
// down by an amount that lands inside the gap after an item, but
// doesn't consume the whole gap. The list must stop mid-gap
// (offsetIdx stays on the preceding item, offsetLine inside its
// trailing gap) rather than jumping straight to the top of the next
// item, which discarded the unconsumed gap remainder and moved the
// viewport further than the requested number of lines.
func TestList_ScrollBy_StopsWithinGapInsteadOfJumping(t *testing.T) {
	t.Parallel()

	a := newMultiLineItem("a", 4)
	b := newMultiLineItem("b", 10)
	l := NewList(a, b)
	l.SetGap(3)
	l.SetSize(20, 5)

	// a's 4 content lines plus 1 line into its trailing 3-line gap: 5
	// lines total, with 2 gap lines still unconsumed before b begins.
	l.ScrollBy(5)
	require.Equal(t, 0, l.offsetIdx, "must remain anchored to a while still inside its trailing gap")
	require.Equal(t, 5, l.offsetLine, "must stop exactly 5 lines down, not skip ahead to b")
}
