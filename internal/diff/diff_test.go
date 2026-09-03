package diff

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGenerateDiff_AddedLineStartsWithPlusPlus covers a content line whose
// own text begins with "++", which renders as "+++…" in the diff body and
// must not be mistaken for the "+++ b/file" header.
func TestGenerateDiff_AddedLineStartsWithPlusPlus(t *testing.T) {
	t.Parallel()

	_, additions, removals := GenerateDiff("a\nb\nc\n", "a\n++b\nc\n", "file.txt")

	require.Equal(t, 1, additions)
	require.Equal(t, 1, removals)
}

// TestGenerateDiff_RemovedLineIsThreeDashes covers a removed content line
// that is exactly "---", which renders as "---…" in the diff body and must
// not be mistaken for the "--- a/file" header. Previously this was
// reported as no change at all.
func TestGenerateDiff_RemovedLineIsThreeDashes(t *testing.T) {
	t.Parallel()

	_, additions, removals := GenerateDiff("a\n---\nc\n", "a\nc\n", "file.txt")

	require.Equal(t, 0, additions)
	require.Equal(t, 1, removals)
}

// TestGenerateDiff_EmptyLineChanges verifies that an added or removed empty
// line still contributes to the counts.
func TestGenerateDiff_EmptyLineChanges(t *testing.T) {
	t.Parallel()

	_, additions, removals := GenerateDiff("a\nb\n", "a\n\nb\n", "file.txt")
	require.Equal(t, 1, additions)
	require.Equal(t, 0, removals)

	_, additions, removals = GenerateDiff("a\n\nb\n", "a\nb\n", "file.txt")
	require.Equal(t, 0, additions)
	require.Equal(t, 1, removals)
}

// TestGenerateDiff_NoNewlineAtEOFMarkerNotCounted verifies that the
// "\ No newline at end of file" marker udiff appends to a hunk is not
// counted as a change on its own.
func TestGenerateDiff_NoNewlineAtEOFMarkerNotCounted(t *testing.T) {
	t.Parallel()

	_, additions, removals := GenerateDiff("a\nb\n", "a\nb\nc", "file.txt")

	require.Equal(t, 1, additions)
	require.Equal(t, 0, removals)
}
