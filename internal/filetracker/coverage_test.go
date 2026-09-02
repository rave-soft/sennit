package filetracker

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRangesRoundTrip covers the storage encoding, including the two
// values that must mean "the whole file was read": the empty string a full
// read writes, and the empty string every row written before ranges
// existed already holds.
//
// The interval arithmetic itself (Covers, Add, Shift, Empty) is tested
// once, against the shared type, in internal/filetracker/coverage.
func TestRangesRoundTrip(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", encodeRanges(FullCoverage))
	require.True(t, decodeRanges("").Full, "a legacy row reads back as fully covered")

	c := Coverage{}.Add(LineRange{Start: 1, End: 50}).Add(LineRange{Start: 400, End: 460})
	require.Equal(t, "[[1,50],[400,460]]", encodeRanges(c))
	require.Equal(t, c.Ranges, decodeRanges(encodeRanges(c)).Ranges)

	require.True(t, decodeRanges("not json").Full, "unparseable ranges fail open, not closed")
}
