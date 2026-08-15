package filetracker

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

// TestRangesRoundTrip covers the storage encoding, including the two
// values that must mean "the whole file was read": the empty string a full
// read writes, and the empty string every row written before ranges
// existed already holds.
func TestRangesRoundTrip(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", encodeRanges(FullCoverage))
	require.True(t, decodeRanges("").Full, "a legacy row reads back as fully covered")

	c := Coverage{}.Add(LineRange{Start: 1, End: 50}).Add(LineRange{Start: 400, End: 460})
	require.Equal(t, "[[1,50],[400,460]]", encodeRanges(c))
	require.Equal(t, c.Ranges, decodeRanges(encodeRanges(c)).Ranges)

	require.True(t, decodeRanges("not json").Full, "unparseable ranges fail open, not closed")
}
