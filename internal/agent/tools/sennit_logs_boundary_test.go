package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadLastLinesKeepsEntriesAcrossChunkBoundaries pins the backwards
// chunked read against the line the boundary cuts. The file is walked in
// 8KB chunks from the end, and the piece carried between iterations has
// to be the chunk's *first* line — the one whose head lies further back.
// Carrying the last line instead dropped it here and carried it forward,
// where it was dropped again: every entry that straddled a boundary
// vanished from the output.
func TestReadLastLinesKeepsEntriesAcrossChunkBoundaries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.log")

	// Entries wide enough that 200 of them span several 8KB chunks.
	const total = 200
	var b strings.Builder
	for i := range total {
		line, err := json.Marshal(map[string]any{
			"time":  "2026-08-23T00:00:00Z",
			"level": "INFO",
			"msg":   fmt.Sprintf("entry-%03d", i),
			"pad":   strings.Repeat("x", 120),
		})
		require.NoError(t, err)
		b.Write(line)
		b.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))

	entries, err := readLastLines(path, 100)
	require.NoError(t, err)
	require.Len(t, entries, 100, "every one of the last 100 entries must survive the chunk walk")

	// Chronological (the function reverses before returning), contiguous,
	// with no gap where a chunk boundary fell.
	for i, entry := range entries {
		require.Equal(t, fmt.Sprintf("entry-%03d", total-100+i), entry["msg"])
	}
}

// --- scanBackward byte-offset arithmetic ------------------------------------
//
// scanBackward's lineEnd for the very first chunk it reads in a call used to
// be initialized to `chunkStart + len(data)`, which equals `start` (the file
// size, or a cursor's stored offset) whenever the read data ends in the
// newline that terminates the newest line in scope - the common case. That
// left every derived lineStart in the first chunk exactly one byte too high,
// because the true content of the newest line ends at the byte *before* that
// newline, not at `start` itself. The offset then propagates correctly
// through every following (already-correct-by-construction) chunk, so the
// whole call's offsets were shifted by one. The fix backs lineEnd up by one
// on the first chunk only, and only when the data actually ends in '\n' (a
// file/cursor boundary with no trailing newline - a file that does not end
// in '\n', or start == 0 - is already exact).

// trueLineOffsets computes each line's true byte offset (its start position
// in the file) independently of scanBackward, for comparison.
func trueLineOffsets(t *testing.T, path string) []int64 {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	offsets := make([]int64, 0, len(lines))
	pos := int64(0)
	for _, l := range lines {
		offsets = append(offsets, pos)
		pos += int64(len(l)) + 1
	}
	return offsets
}

// scanBackwardAll opens path and returns scanBackward's full result.
func scanBackwardAll(t *testing.T, path string, limit int) backwardScanResult {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	return scanBackward(f, info.Size(), -1, mustLogFilter(), limit)
}

// TestScanBackwardOffsetsAreExact pins the corrected offsets on a minimal
// 3-line fixture: every entry's recorded offset must equal its true byte
// position in the file, not one past it.
func TestScanBackwardOffsetsAreExact(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "line0", nil),
		entryLine("INFO", "line1", nil),
		entryLine("INFO", "line2", nil),
	)
	want := trueLineOffsets(t, path)
	require.Len(t, want, 3)

	result := scanBackwardAll(t, path, 10)
	require.Len(t, result.entries, 3)
	for i, rec := range result.entries {
		require.Equal(t, want[i], rec.offset, "line %d offset", i)
	}
}

// TestScanBackwardOffsetsNoTrailingNewline pins that a file whose last byte is
// real content (no trailing newline) is unaffected by the first-chunk fix:
// start already sits exactly at the true end of the last line.
func TestScanBackwardOffsetsNoTrailingNewline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.log")
	l0 := entryLine("INFO", "a", nil)
	l1 := entryLine("INFO", "b", nil)
	require.NoError(t, os.WriteFile(path, []byte(l0+"\n"+l1), 0o600)) // no trailing "\n"

	result := scanBackwardAll(t, path, 10)
	require.Len(t, result.entries, 2)
	require.Equal(t, int64(0), result.entries[0].offset)
	require.Equal(t, int64(len(l0)+1), result.entries[1].offset)
}

// TestScanBackwardOffsetsSingleLine pins the smallest non-empty case: one
// line, offset 0.
func TestScanBackwardOffsetsSingleLine(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t, entryLine("INFO", "only", nil))
	result := scanBackwardAll(t, path, 10)
	require.Len(t, result.entries, 1)
	require.Equal(t, int64(0), result.entries[0].offset)
}

// TestScanBackwardOffsetsEmptyFile pins that an empty file yields no entries
// and is trivially a complete (reachedStart) scan.
func TestScanBackwardOffsetsEmptyFile(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t)
	result := scanBackwardAll(t, path, 10)
	require.Empty(t, result.entries)
	require.True(t, result.reachedStart)
}

// TestScanBackwardOffsetsAcrossChunkBoundary extends the offset check to a
// file spanning several 8KB chunks, so the first-chunk fix and the
// pre-existing remainder-carrying logic are both exercised together.
func TestScanBackwardOffsetsAcrossChunkBoundary(t *testing.T) {
	t.Parallel()
	const total = 200
	var b strings.Builder
	for i := range total {
		line, err := json.Marshal(map[string]any{
			"time": "2026-08-23T00:00:00Z", "level": "INFO",
			"msg": fmt.Sprintf("entry-%03d", i), "pad": strings.Repeat("x", 120),
		})
		require.NoError(t, err)
		b.Write(line)
		b.WriteByte('\n')
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.log")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))

	want := trueLineOffsets(t, path)
	result := scanBackwardAll(t, path, total)
	require.Len(t, result.entries, total)
	for i, rec := range result.entries {
		require.Equal(t, want[i], rec.offset, "line %d offset", i)
	}
}

// TestScanBackwardCursorContinuationAtChunkBoundary resumes a cursor whose
// offset lands several 8KB chunks deep, so the continuation's own first-chunk
// read starts at a genuine chunk-sized remove from both file ends. Pins that
// the fix applies per-call (based on the function's start parameter), not
// merely on chunkStart == 0.
func TestScanBackwardCursorContinuationAtChunkBoundary(t *testing.T) {
	t.Parallel()
	const total = 400
	var b strings.Builder
	for range total {
		b.WriteString(entryLine("INFO", "padpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpad", nil))
		b.WriteByte('\n')
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.log")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
	want := trueLineOffsets(t, path)

	// A cursor several chunks deep from the end.
	var cursor int64 = -1
	for _, off := range want {
		if off > 3*8192 {
			cursor = off
			break
		}
	}
	require.GreaterOrEqual(t, cursor, int64(0), "fixture must be large enough for a multi-chunk cursor")

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	result := scanBackward(f, cursor, cursor, mustLogFilter(), 5)
	require.Len(t, result.entries, 5)
	// The newest entry in this continuation is the one immediately before the
	// cursor's line, never the cursor's own (already-returned) line.
	require.Less(t, result.entries[len(result.entries)-1].offset, cursor)
}

// TestScanBackwardAlreadyReturnedIsUnreachable documents and pins an
// investigation finding: alreadyReturned (boundary >= 0 && lineStart ==
// boundary) can never fire, independent of the offset fix above. A
// continuation's chunk reads always cover [chunkStart, pos) with pos
// initialized to start (== boundary), so every lineStart the scan derives is
// strictly less than boundary by construction - the boundary's own line sits
// just outside the read window. This was true before the offset fix too (the
// old off-by-one made offsets one too high, i.e. even further from
// boundary, never equal to it), so fixing the offset does not "revive" this
// branch; it remains long-standing, harmless dead code. This test walks a
// full two-page pagination (mirroring TestSennitLogs_PaginationNoDupNoGap)
// and asserts no entry from either page ever lands on the cursor's own
// boundary offset, which is the only way alreadyReturned's guard could ever
// matter.
func TestScanBackwardAlreadyReturnedIsUnreachable(t *testing.T) {
	t.Parallel()
	const total = 20
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("e%02d", i), nil))
	}
	path := writeRawLog(t, lines...)

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)

	page1 := scanBackward(f, info.Size(), -1, mustLogFilter(), 10)
	require.Len(t, page1.entries, 10)
	boundary := page1.entries[0].offset // oldest of page1: the next cursor

	page2 := scanBackward(f, boundary, boundary, mustLogFilter(), 10)
	require.Len(t, page2.entries, 10)
	for _, rec := range page2.entries {
		require.Less(t, rec.offset, boundary,
			"a continuation must never read the boundary's own (already-returned) line")
	}
	// No duplicate and no gap across the two pages together.
	seen := map[int64]bool{}
	for _, rec := range append(append([]logRecord{}, page1.entries...), page2.entries...) {
		require.False(t, seen[rec.offset], "offset %d duplicated across pages", rec.offset)
		seen[rec.offset] = true
	}
	require.Len(t, seen, total)
}
