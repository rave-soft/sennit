package diffview

// The setters below are exported so diffview_test.go (an external test
// package) can chain them the same way a real caller would, but nothing in
// production sets these particular options today — deadcode confirmed
// each is reachable only from tests. They live here, in an internal test
// file, rather than diffview.go, because they still need direct field
// access (and, for ContextLines, clearCaches) that only package diffview
// itself has.

// ContextLines sets the number of context lines for the DiffView.
func (dv *DiffView) ContextLines(contextLines int) *DiffView {
	dv.contextLines = contextLines
	// computeDiff caches its result behind isComputed; without dropping
	// that here, a ContextLines() call after the first String() would
	// silently keep the diff computed under the old value.
	dv.clearCaches()
	return dv
}

// LineNumbers sets whether to display line numbers in the DiffView.
func (dv *DiffView) LineNumbers(lineNumbers bool) *DiffView {
	dv.lineNumbers = lineNumbers
	return dv
}

// Height sets the height of the DiffView.
func (dv *DiffView) Height(height int) *DiffView {
	dv.height = height
	return dv
}

// YOffset sets the vertical offset for the DiffView.
func (dv *DiffView) YOffset(yOffset int) *DiffView {
	dv.yOffset = yOffset
	return dv
}

// InfiniteYScroll allows the YOffset to scroll beyond the last line.
func (dv *DiffView) InfiniteYScroll(infiniteYScroll bool) *DiffView {
	dv.infiniteYScroll = infiniteYScroll
	return dv
}
