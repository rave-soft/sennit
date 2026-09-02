package coverage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCoverageCovers is the rule an edit is checked against: only lines
// actually served by a read count as read.
func TestCoverageCovers(t *testing.T) {
	t.Parallel()

	require.True(t, FullCoverage.Covers(1, 10_000), "a whole-file read covers everything")
	require.False(t, Coverage{}.Covers(1, 1), "never read, nothing covered")

	c := Coverage{}.Add(LineRange{Start: 1, End: 50}).Add(LineRange{Start: 400, End: 460})
	require.True(t, c.Covers(1, 50))
	require.True(t, c.Covers(10, 20))
	require.True(t, c.Covers(400, 460))
	require.False(t, c.Covers(51, 60), "the gap between windows was never read")
	require.False(t, c.Covers(40, 60), "a span running out of a window is not covered")
	require.False(t, c.Covers(1900, 1900), "past everything read")
}

// TestCoverageAddMergesAdjacent proves two reads that meet or overlap fuse
// into one span, so an edit straddling the seam is allowed.
func TestCoverageAddMergesAdjacent(t *testing.T) {
	t.Parallel()

	c := Coverage{}.Add(LineRange{Start: 1, End: 50}).Add(LineRange{Start: 51, End: 100})
	require.Equal(t, []LineRange{{Start: 1, End: 100}}, c.Ranges)
	require.True(t, c.Covers(45, 55))

	overlapping := Coverage{}.Add(LineRange{Start: 10, End: 20}).Add(LineRange{Start: 15, End: 40})
	require.Equal(t, []LineRange{{Start: 10, End: 40}}, overlapping.Ranges)
}

// TestCoverageAddKeepsDisjointRangesSorted proves out-of-order reads still
// produce a sorted, gap-preserving range list.
func TestCoverageAddKeepsDisjointRangesSorted(t *testing.T) {
	t.Parallel()

	c := Coverage{}.Add(LineRange{Start: 400, End: 460}).Add(LineRange{Start: 1, End: 50})
	require.Equal(t, []LineRange{{Start: 1, End: 50}, {Start: 400, End: 460}}, c.Ranges)
}

// TestCoverageAddOnFullStaysFull proves a partial read after a full one
// cannot narrow what is already known to be covered.
func TestCoverageAddOnFullStaysFull(t *testing.T) {
	t.Parallel()

	require.True(t, FullCoverage.Add(LineRange{Start: 1, End: 5}).Full)
}

// TestCoverageEmpty proves Empty tracks "nothing read at all", not "no
// ranges recorded" — a full read has no ranges but is not empty.
func TestCoverageEmpty(t *testing.T) {
	t.Parallel()

	require.True(t, Coverage{}.Empty())
	require.False(t, FullCoverage.Empty())
	require.False(t, Coverage{}.Add(LineRange{Start: 1, End: 1}).Empty())
}

// TestCoverageShift proves coverage follows the text it describes when an
// edit changes how many lines precede it. Without this, editing near the
// top of a file leaves every range below it pointing at the wrong lines.
func TestCoverageShift(t *testing.T) {
	t.Parallel()

	c := Coverage{}.Add(LineRange{Start: 1, End: 20}).Add(LineRange{Start: 100, End: 120})

	// Line 10 became three lines: everything wholly below moves down two,
	// the range containing the edit absorbs the delta.
	shifted := c.Shift(10, 10, 2)
	require.Equal(t, []LineRange{{Start: 1, End: 22}, {Start: 102, End: 122}}, shifted.Ranges)

	// An edit below a range leaves it alone.
	require.Equal(t, []LineRange{{Start: 1, End: 20}, {Start: 100, End: 125}},
		c.Shift(110, 110, 5).Ranges)

	// A full read stays full, and a no-op delta changes nothing.
	require.True(t, FullCoverage.Shift(1, 1, 5).Full)
	require.Equal(t, c.Ranges, c.Shift(10, 10, 0).Ranges)
}

// TestCoverageShiftKeepsWhatWasReadBelowAShrinkingEdit pins the case the
// old clamp destroyed. A range whose start lies inside a shrinking edit
// had its end pushed below its start and was then clamped up to it,
// collapsing the whole range to a single line: every line the session had
// actually read below the edit was recorded as unread, and the next edit
// to those lines was refused with "read the file first".
func TestCoverageShiftKeepsWhatWasReadBelowAShrinkingEdit(t *testing.T) {
	t.Parallel()

	// The session read lines 80-100. An edit replaces 10-90 with a single
	// line (delta -80), so what it read now lives at 10-20.
	got := Coverage{Ranges: []LineRange{{Start: 80, End: 100}}}.Shift(10, 90, -80)

	require.Equal(t, []LineRange{{Start: 10, End: 20}}, got.Ranges)
}

// TestCoverageShiftDropsARangeTheEditRemovedEntirely keeps the other
// half honest: coverage of lines that no longer exist is not coverage.
func TestCoverageShiftDropsARangeTheEditRemovedEntirely(t *testing.T) {
	t.Parallel()

	// Lines 20-30 sat inside an edit that deleted 10-30 outright.
	got := Coverage{Ranges: []LineRange{{Start: 20, End: 30}}}.Shift(10, 30, -21)

	require.Empty(t, got.Ranges)
}

// TestCoverageShiftSpanningRangeAbsorbsTheDelta covers the ordinary case:
// a range spanning the edit keeps its start and absorbs the delta.
func TestCoverageShiftSpanningRangeAbsorbsTheDelta(t *testing.T) {
	t.Parallel()

	got := Coverage{Ranges: []LineRange{{Start: 1, End: 100}}}.Shift(10, 20, -5)

	require.Equal(t, []LineRange{{Start: 1, End: 95}}, got.Ranges)
}
