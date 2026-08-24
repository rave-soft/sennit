package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// --- helpers for the T5 tests ---------------------------------------------

// writeRawLog writes arbitrary raw lines (not necessarily valid JSON) to a
// fresh temp file and returns its path. Used to craft malformed/oversized/
// rotation scenarios that the structured entry writer cannot express.
func writeRawLog(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sennit.log")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
	return path
}

// appendLog appends raw lines to an existing log file (for the appended-file
// and rotation tests).
func appendLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer f.Close()
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	_, err = f.WriteString(b.String())
	require.NoError(t, err)
}

// entryLine marshals a log entry to a single JSON line.
func entryLine(level, msg string, extra map[string]any) string {
	entry := map[string]any{
		"time":  "2024-01-15T10:30:00Z",
		"level": level,
		"msg":   msg,
		"source": map[string]any{
			"file": "app.go",
			"line": 1,
		},
	}
	for k, v := range extra {
		entry[k] = v
	}
	b, _ := json.Marshal(entry)
	return string(b)
}

// runText calls runSennitLogsText and splits the result into lines, dropping
// the trailing metadata footer so the assertions operate on entry lines only.
// The "no matches" sentinel messages (which carry no entry lines) become nil.
func runText(t *testing.T, path string, p SennitLogsParams) []string {
	t.Helper()
	out := runSennitLogsText(path, p)
	if out == "" || strings.HasPrefix(out, "No log") || strings.HasPrefix(out, "No more") {
		return nil
	}
	return strings.Split(out, "\n")
}

// runFull returns the (text, metadata, error) from the tool-facing path, which
// keeps the metadata footer in the text.
func runFull(t *testing.T, path string, p SennitLogsParams) (string, SennitLogsResponseMetadata, error) {
	t.Helper()
	return runSennitLogs(path, p)
}

// --- filters ---------------------------------------------------------------

func TestSennitLogs_FilterByLevel(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("DEBUG", "dbg", nil),
		entryLine("INFO", "inf", nil),
		entryLine("WARN", "warn1", nil),
		entryLine("ERROR", "err", nil),
		entryLine("WARNING", "warn2", nil),
	)
	// Only WARN (the parser normalizes WARNING entries too).
	lines := runText(t, path, SennitLogsParams{Level: "WARN", Limit: 50})
	require.Len(t, lines, 2)
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "warn1")
	require.Contains(t, joined, "warn2")
	require.NotContains(t, joined, "dbg")
	require.NotContains(t, joined, "inf")
	require.NotContains(t, joined, "err")
}

func TestSennitLogs_FilterByComponent(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "agent line", map[string]any{"component": "agent"}),
		entryLine("INFO", "skills line", map[string]any{"component": "skills"}),
		entryLine("INFO", "no component", nil),
	)
	lines := runText(t, path, SennitLogsParams{Component: "agent", Limit: 50})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "agent line")
}

func TestSennitLogs_FilterByContains(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "Provider request failed with timeout", nil),
		entryLine("INFO", "everything is fine", nil),
		entryLine("ERROR", "provider error: connection refused", nil),
	)
	// Case-insensitive substring over the message.
	lines := runText(t, path, SennitLogsParams{Contains: "provider", Limit: 50})
	require.Len(t, lines, 2)
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "timeout")
	require.Contains(t, joined, "connection refused")
	require.NotContains(t, joined, "fine")
}

func TestSennitLogs_ContainsDoesNotSearchRedactedValues(t *testing.T) {
	t.Parallel()
	// A secret value that contains the needle must NOT satisfy the filter:
	// contains must not become a way to exfiltrate a redacted secret.
	path := writeRawLog(t,
		entryLine("INFO", "auth call", map[string]any{"authorization": "Bearer topsecret-needle"}),
		entryLine("INFO", "no secret here", nil),
	)
	// "topsecret-needle" appears only in the redacted authorization value.
	lines := runText(t, path, SennitLogsParams{Contains: "topsecret-needle", Limit: 50})
	require.Empty(t, lines, "a contains filter must not match a redacted field value")
}

func TestSennitLogs_FilterBySessionAndRun(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "s1 r1 a", map[string]any{"session_id": "s1", "run_id": "r1"}),
		entryLine("INFO", "s1 r2 b", map[string]any{"session_id": "s1", "run_id": "r2"}),
		entryLine("INFO", "s2 r1 c", map[string]any{"session_id": "s2", "run_id": "r1"}),
		entryLine("INFO", "no ids", nil),
	)
	lines := runText(t, path, SennitLogsParams{SessionID: "s1", RunID: "r2", Limit: 50})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "s1 r2 b")
}

func TestSennitLogs_FilterBySince(t *testing.T) {
	t.Parallel()
	base := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	mk := func(off time.Duration, msg string) string {
		return entryLine("INFO", msg, map[string]any{
			"time": base.Add(off).Format(time.RFC3339),
		})
	}
	// entryLine sets time but we override it via extra (maps.Copy wins).
	path := writeRawLog(t,
		mk(0, "t0"),
		mk(time.Minute, "t1m"),
		mk(2*time.Minute, "t2m"),
		mk(3*time.Minute, "t3m"),
	)
	// since = t1m -> keep t1m, t2m, t3m (>=).
	lines := runText(t, path, SennitLogsParams{Since: base.Add(time.Minute).Format(time.RFC3339), Limit: 50})
	require.Len(t, lines, 3)
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "t1m")
	require.Contains(t, joined, "t2m")
	require.Contains(t, joined, "t3m")
	require.NotContains(t, joined, "t0")

	// Relative since (1m ago) on a file of old entries -> all are before now,
	// so nothing survives; assert it does not error and returns no lines.
	pathOld := writeRawLog(t, entryLine("INFO", "old", map[string]any{"time": "2020-01-01T00:00:00Z"}))
	lines2 := runText(t, pathOld, SennitLogsParams{Since: "1m", Limit: 50})
	require.Empty(t, lines2, "a relative since must exclude an old entry")
}

func TestSennitLogs_CombinedFilters(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("ERROR", "db timeout", map[string]any{"session_id": "s1", "run_id": "r1"}),
		entryLine("ERROR", "db full", map[string]any{"session_id": "s1", "run_id": "r1"}),
		entryLine("ERROR", "db timeout", map[string]any{"session_id": "s2", "run_id": "r1"}),
		entryLine("INFO", "db timeout", map[string]any{"session_id": "s1", "run_id": "r1"}),
	)
	lines := runText(t, path, SennitLogsParams{Level: "ERROR", Contains: "timeout", SessionID: "s1", Limit: 50})
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "db timeout")
}

// --- pagination (no dup / no gap) ------------------------------------------

func TestSennitLogs_PaginationNoDupNoGap(t *testing.T) {
	t.Parallel()
	// 1000 entries, each uniquely numbered.
	const total = 1000
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("Entry %03d", i), nil))
	}
	path := writeRawLog(t, lines...)

	const pageSize = 250
	var pageIdx []int
	pageNo := 0
	cursor := ""
	for {
		out, meta, err := runFull(t, path, SennitLogsParams{Limit: pageSize, Cursor: cursor})
		require.NoError(t, err)
		require.Greater(t, meta.ShownCount, 0, "page %d must be non-empty", pageNo)
		// The page's entries, in chronological (oldest-first) order within the page.
		for _, l := range strings.Split(stripMetaFooter(out), "\n") {
			pageIdx = append(pageIdx, parseEntryNo(l))
		}
		pageNo++
		if !meta.Truncated || meta.NextCursor == "" {
			break
		}
		cursor = meta.NextCursor
		if pageNo > 20 {
			t.Fatal("too many pages; pagination is not terminating")
		}
	}

	// Every entry exactly once (no dup), covering the whole file (no gap).
	require.Len(t, pageIdx, total, "the whole file must be covered exactly once")
	seen := map[int]bool{}
	for _, i := range pageIdx {
		require.False(t, seen[i], "duplicate index %d across pages", i)
		seen[i] = true
	}
	// The union must be exactly [0, total).
	for i := range total {
		require.True(t, seen[i], "index %d was never returned (gap)", i)
	}
	// Pages are newest-first, each internally chronological. Reassemble
	// oldest-first and confirm it is exactly 0,1,2,...,total-1 (no gap, no
	// dup, correct global order).
	var allPages [][]int
	pos := 0
	for n := 0; n < pageNo; n++ {
		end := pos + pageSize
		if end > len(pageIdx) {
			end = len(pageIdx)
		}
		allPages = append(allPages, pageIdx[pos:end])
		pos = end
	}
	var rebuilt []int
	for i := len(allPages) - 1; i >= 0; i-- {
		rebuilt = append(rebuilt, allPages[i]...)
	}
	require.Equal(t, total, len(rebuilt))
	for i := range rebuilt {
		require.Equal(t, i, rebuilt[i], "reassembled order must be 0,1,2,... (no gap)")
	}
}

// parseEntryNo extracts the trailing "Entry %03d" number from a formatted line.
func parseEntryNo(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return -1
	}
	// The msg "Entry 012" is the last two fields: "Entry", "012".
	var n int
	_, _ = fmt.Sscanf(fields[len(fields)-1], "%d", &n)
	return n
}

func TestSennitLogs_PaginationRespectsFilter(t *testing.T) {
	t.Parallel()
	// 600 entries, every 3rd is an ERROR (the filter target).
	const total = 600
	var lines []string
	errCount := 0
	for i := range total {
		if i%3 == 0 {
			lines = append(lines, entryLine("ERROR", fmt.Sprintf("err %03d", i), nil))
			errCount++
		} else {
			lines = append(lines, entryLine("INFO", fmt.Sprintf("info %03d", i), nil))
		}
	}
	path := writeRawLog(t, lines...)

	const pageSize = 50
	var seenErr []int
	cursor := ""
	matched := 0
	for {
		out, meta, err := runFull(t, path, SennitLogsParams{Level: "ERROR", Limit: pageSize, Cursor: cursor})
		require.NoError(t, err)
		require.Equal(t, errCount, meta.MatchCount, "match_count must be the total filtered matches, stable across pages")
		for _, l := range strings.Split(stripMetaFooter(out), "\n") {
			seenErr = append(seenErr, parseIndex(l))
		}
		matched = meta.MatchCount
		if !meta.Truncated || meta.NextCursor == "" {
			break
		}
		cursor = meta.NextCursor
	}
	require.Len(t, seenErr, errCount)
	require.Equal(t, errCount, matched)
	// No duplicates.
	set := map[int]bool{}
	for _, i := range seenErr {
		require.False(t, set[i], "duplicate index %d in paginated filtered results", i)
		set[i] = true
	}
}

func parseIndex(line string) int {
	// The msg is "err %03d" and is the second-to-last field; the line ends
	// with "err 012" -> fields [..., "err", "012"]. Take the last field.
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return -1
	}
	var n int
	_, _ = fmt.Sscanf(fields[len(fields)-1], "%d", &n)
	return n
}

// --- appended file: cursor stable ------------------------------------------

func TestSennitLogs_CursorStableUnderAppends(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 20 {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("old %02d", i), nil))
	}
	path := writeRawLog(t, lines...)

	// First call: the 10 most recent (old 10..19), capturing a cursor.
	out, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	firstPage := strings.Split(stripMetaFooter(out), "\n")
	require.Len(t, firstPage, 10)
	require.True(t, meta.Truncated)
	cursor := meta.NextCursor
	require.NotEmpty(t, cursor)

	// Append 5 NEW entries after the cursor.
	for i := range 5 {
		appendLog(t, path, entryLine("INFO", fmt.Sprintf("new %d", i), nil))
	}

	// Continuing with the same cursor must return the SAME older entries
	// (old 0..9), NOT the new ones and NOT a re-shuffled mix: appends sit
	// beyond the cursor's byte offset and are not scanned.
	out2, meta2, err := runFull(t, path, SennitLogsParams{Limit: 10, Cursor: cursor})
	require.NoError(t, err)
	secondPage := strings.Split(stripMetaFooter(out2), "\n")
	require.Len(t, secondPage, 10)
	joined := strings.Join(secondPage, "\n")
	for i := range 10 {
		require.Contains(t, joined, fmt.Sprintf("old %02d", i),
			"continuing the cursor must yield the same older entries")
	}
	require.NotContains(t, joined, "new 0", "appended entries must not appear in a cursor continuation")
	_ = meta2
}

func TestSennitLogs_NoCursorSeesAppends(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "base one", nil),
		entryLine("INFO", "base two", nil),
	)
	appendLog(t, path, entryLine("INFO", "appended", nil))
	// A fresh (no-cursor) call sees the appended entry as the most recent.
	lines := runText(t, path, SennitLogsParams{Limit: 10})
	require.Len(t, lines, 3)
	require.Contains(t, lines[2], "appended")
}

// --- rotation --------------------------------------------------------------

func TestSennitLogs_CursorAfterRotationReturnsEmpty(t *testing.T) {
	t.Parallel()
	// A big file (several 8KB chunks) so the cursor points mid-file.
	var lines []string
	for i := range 300 {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("rot %03d", i), map[string]any{
			"pad": strings.Repeat("x", 100),
		}))
	}
	path := writeRawLog(t, lines...)
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	cursor := meta.NextCursor
	require.NotEmpty(t, cursor)

	// Rotate: replace the file with a DIFFERENT log (a new inode, not just a
	// truncation). The cursor is bound to the old file's generation; the new
	// file is a different generation, so the continuation is STALE. It must
	// return an empty page with stale-page metadata and must NOT emit any new
	// generation's records (block 1).
	require.NoError(t, os.WriteFile(path, []byte(entryLine("INFO", "rotated fresh", nil)+"\n"), 0o600))
	out, meta2, err := runFull(t, path, SennitLogsParams{Limit: 10, Cursor: cursor})
	require.NoError(t, err)
	// Stale page: empty, not truncated, no next cursor, nothing matched.
	require.Equal(t, 0, meta2.ShownCount)
	require.False(t, meta2.Truncated)
	require.Empty(t, meta2.NextCursor)
	require.Equal(t, 0, meta2.MatchCount)
	// No new generation records leaked in: the fresh "rotated fresh" entry must
	// NOT appear (a stale token is never treated as a fresh continuation).
	require.NotContains(t, out, "rotated fresh")
	// Not an error; the text explains the cursor went stale.
	require.NotContains(t, out, "error")
}

// TestSennitLogs_CursorStaleOnOffsetBeyondSize pins the "old offset > size"
// half of the stale rule (block 1): a cursor whose byte offset now exceeds the
// file size (the file shrank but kept the same identity, or simply became
// shorter than the recorded offset) is stale and returns an empty page.
func TestSennitLogs_CursorStaleOnOffsetBeyondSize(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 50 {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("s %02d", i), map[string]any{
			"pad": strings.Repeat("y", 200),
		}))
	}
	path := writeRawLog(t, lines...)
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	cursor := meta.NextCursor
	require.NotEmpty(t, cursor)

	// Shrink the file to a few bytes (same path). The cursor's offset now
	// exceeds the new size -> stale -> empty page, no records, not an error.
	require.NoError(t, os.WriteFile(path, []byte(entryLine("INFO", "shrunken", nil)+"\n"), 0o600))
	out, meta2, err := runFull(t, path, SennitLogsParams{Limit: 10, Cursor: cursor})
	require.NoError(t, err)
	require.Equal(t, 0, meta2.ShownCount)
	require.False(t, meta2.Truncated)
	require.Empty(t, meta2.NextCursor)
	require.NotContains(t, out, "shrunken")
	require.NotContains(t, out, "error")
}

// TestSennitLogs_CursorSameFileContinues pins the POSITIVE case for the
// generation check: continuing with a cursor on the SAME file (same inode,
// grown by appends beyond the cursor) is NOT stale and returns the older
// page. This is the counterpart to the two stale tests above and is what
// proves the identity check is not over-eager.
func TestSennitLogs_CursorSameFileContinues(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 20 {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("base %02d", i), nil))
	}
	path := writeRawLog(t, lines...)
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 5})
	require.NoError(t, err)
	cursor := meta.NextCursor
	require.NotEmpty(t, cursor)

	// Append MORE entries to the SAME file (same inode). The cursor stays
	// valid (same generation) and the continuation returns the next older page.
	appendLog(t, path,
		entryLine("INFO", "appended a", nil),
		entryLine("INFO", "appended b", nil),
	)
	out, meta2, err := runFull(t, path, SennitLogsParams{Limit: 5, Cursor: cursor})
	require.NoError(t, err)
	require.Greater(t, meta2.ShownCount, 0, "a same-file continuation must return the older page, not be treated as stale")
	// The first page was base 15-19 (most recent 5); the continuation is the
	// next older page, base 10-14. It must NOT include the first page's entries
	// (no dup) and NOT the appended ones (appends sit beyond the cursor).
	joined := stripMetaFooter(out)
	require.Contains(t, joined, "base 10")
	require.Contains(t, joined, "base 14")
	require.NotContains(t, joined, "base 15", "the cursor's own oldest entry must not be re-returned")
	require.NotContains(t, joined, "base 19")
	require.NotContains(t, joined, "appended a", "appended entries must not appear in a same-file continuation")
}

// --- malformed / oversized / partial ---------------------------------------

func TestSennitLogs_SkipsMalformedAndOversized(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("y", maxLogLineSize+1000)
	path := writeRawLog(t,
		entryLine("INFO", "good one", nil),
		"this is not json at all",
		`{"incomplete": "json`,
		fmt.Sprintf(`{"time":"2024-01-15T10:30:00Z","level":"INFO","msg":"big","data":"%s"}`, big),
		entryLine("ERROR", "good two", nil),
	)
	lines := runText(t, path, SennitLogsParams{Limit: 50})
	require.Len(t, lines, 2, "only the 2 valid entries survive; malformed+oversized are skipped")
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "good one")
	require.Contains(t, joined, "good two")
	require.NotContains(t, joined, "big")
	require.NotContains(t, joined, "not json")
}

// TestSennitLogs_OversizedStraddlesChunkBoundaryIsDiscardedStreaming pins the
// block-4 streaming discard: an oversized line that straddles 8KB chunk
// boundaries must be discarded without accumulating the whole line in the
// remainder (unbounded for a multi-MB line), and the valid entries around it
// must survive. The oversized line is built to be > 2 chunks wide so a
// naive carry would accumulate the whole thing.
func TestSennitLogs_OversizedStraddlesChunkBoundaryIsDiscardedStreaming(t *testing.T) {
	t.Parallel()
	// A line much larger than maxLogLineSize (1MB): ~1.4MB so it spans many
	// 8KB chunks. Placed between valid entries.
	big := `{"time":"2024-01-15T10:30:00Z","level":"INFO","msg":"huge","data":"` + strings.Repeat("q", maxLogLineSize+128*1024) + `"}`
	path := writeRawLog(t,
		entryLine("INFO", "before huge", nil),
		big,
		entryLine("ERROR", "after huge", nil),
	)
	lines := runText(t, path, SennitLogsParams{Limit: 50})
	require.Len(t, lines, 2, "the oversized line is discarded streaming; both valid entries survive")
	joined := strings.Join(lines, "\n")
	require.Contains(t, joined, "before huge")
	require.Contains(t, joined, "after huge")
	require.NotContains(t, joined, "huge\"")
	// The scan is reported as not-exact (an oversized line was discarded):
	// match_count_exact is false, scanned_truncated is true.
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 50})
	require.NoError(t, err)
	require.False(t, meta.MatchCountExact, "discarding an oversized line means the file was not fully scanned")
	require.True(t, meta.ScannedTruncated)
}

// --- backward lines: 8KB boundary ------------------------------------------

// TestSennitLogs_BackwardChunkBoundary pins that an entry straddling an 8KB
// chunk boundary is not dropped (the pre-T5 remainder-carrying bug). This is
// the backward-read regression the boundary test already pins for
// readLastLines; it must hold through the full runSennitLogs path too.
func TestSennitLogs_BackwardChunkBoundary(t *testing.T) {
	t.Parallel()
	const total = 200
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("entry-%03d", i), map[string]any{
			"pad": strings.Repeat("x", 120),
		}))
	}
	path := writeRawLog(t, lines...)
	// Read the last 100 with a limit that forces several chunk walks.
	linesOut := runText(t, path, SennitLogsParams{Limit: 100})
	require.Len(t, linesOut, 100)
	for i, l := range linesOut {
		require.Contains(t, l, fmt.Sprintf("entry-%03d", total-100+i),
			"an entry at a chunk boundary was dropped (index %d)", i)
	}
}

// --- chain mode ------------------------------------------------------------

// chainFixture builds a log file with a correlated chain for one run plus
// noise that is not part of the chain (routine INFOs, other sessions).
func chainFixture(t *testing.T) string {
	t.Helper()
	const sess, run = "sess-1", "run-1"
	mk := func(msg, level string, extra map[string]any) string {
		e := map[string]any{
			"time":       "2024-01-15T10:30:00Z",
			"level":      level,
			"msg":        msg,
			"session_id": sess,
			"run_id":     run,
		}
		for k, v := range extra {
			e[k] = v
		}
		b, _ := json.Marshal(e)
		return string(b)
	}
	lines := []string{
		// Chain anchors for this run.
		mk("provider request started", "INFO", map[string]any{"step": 1, "attempt": 1, "request_reason": "turn"}),
		mk("provider request failed, retrying", "WARN", map[string]any{"step": 1, "attempt": 1, "retry_reason": "rate_limited"}),
		mk("provider request started", "INFO", map[string]any{"step": 1, "attempt": 2, "request_reason": "retry"}),
		mk("provider request finished", "INFO", map[string]any{"step": 1, "attempt": 2, "outcome": "success"}),
		mk("Trimmed the carried sub-agent session to the budget", "INFO", map[string]any{"dropped_messages": 3}),
		mk("Injecting synthetic tool result for orphaned tool call", "WARN", map[string]any{"tool_call_id": "call_1"}),
		// Noise: same session/run but not an anchor.
		mk("Completion enqueued", "INFO", nil),
		mk("Steering folded into turn", "INFO", nil),
		// Noise: a different run in the same session.
		mk("provider request started", "INFO", map[string]any{"run_id": "run-2"}),
		// Noise: a different session.
		mk("provider request started", "INFO", map[string]any{"session_id": "sess-2"}),
	}
	return writeRawLog(t, lines...)
}

func TestSennitLogs_ChainModeReturnsOnlyAnchors(t *testing.T) {
	t.Parallel()
	path := chainFixture(t)
	out, meta, err := runFull(t, path, SennitLogsParams{Chain: true, SessionID: "sess-1", RunID: "run-1"})
	require.NoError(t, err)
	require.True(t, meta.ChainMode)
	require.Equal(t, "sess-1", meta.ChainSession)
	require.Equal(t, "run-1", meta.ChainRun)

	body := stripMetaFooter(out)
	lines := strings.Split(body, "\n")
	// The first line is the chain banner; the rest are the anchor entries.
	require.Contains(t, lines[0], "correlated chain")
	entryLines := lines[1:]
	// 6 anchors for sess-1/run-1 (2 started, 1 retry, 1 finished, 1 trim, 1 repair).
	require.Len(t, entryLines, 6)
	joined := strings.Join(entryLines, "\n")
	// Only anchors, no noise.
	require.Contains(t, joined, "provider request started")
	require.Contains(t, joined, "provider request failed, retrying")
	require.Contains(t, joined, "Trimmed the carried")
	require.Contains(t, joined, "Injecting synthetic")
	require.NotContains(t, joined, "Completion enqueued", "non-anchor noise must be excluded")
	require.NotContains(t, joined, "Steering folded", "non-anchor noise must be excluded")
	// The other-session / other-run anchors are excluded by the session/run filter.
	require.NotContains(t, joined, "sess-2")
}

func TestSennitLogs_ChainModeRespectsSessionFilterOnly(t *testing.T) {
	t.Parallel()
	path := chainFixture(t)
	// Chain by session only: includes run-2's anchor for sess-1 but not sess-2.
	out, meta, err := runFull(t, path, SennitLogsParams{Chain: true, SessionID: "sess-1"})
	require.NoError(t, err)
	require.True(t, meta.ChainMode)
	body := stripMetaFooter(out)
	// sess-1 anchors: 6 (run-1) + 1 (run-2's started) = 7.
	lines := strings.Split(body, "\n")[1:] // drop banner
	require.Len(t, lines, 7)
	require.NotContains(t, body, "sess-2")
}

// --- metadata correctness --------------------------------------------------

func TestSennitLogs_MetadataMatchAndTruncated(t *testing.T) {
	t.Parallel()
	const total = 300
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("Entry %03d", i), nil))
	}
	path := writeRawLog(t, lines...)

	// Page of 100 from a 300-entry file: 100 shown, 200 matched-behind,
	// truncated, with a next cursor.
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, total, meta.MatchCount)
	require.Equal(t, 100, meta.ShownCount)
	require.True(t, meta.Truncated)
	require.NotEmpty(t, meta.NextCursor)

	// A page larger than the file: all shown, not truncated, no next cursor.
	_, meta2, err := runFull(t, path, SennitLogsParams{Limit: maxLogLines})
	require.NoError(t, err)
	require.Equal(t, total, meta2.MatchCount)
	require.Equal(t, total, meta2.ShownCount)
	require.False(t, meta2.Truncated)
	require.Empty(t, meta2.NextCursor)
}

func TestSennitLogs_MetadataFooterInText(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t,
		entryLine("INFO", "one", nil),
		entryLine("INFO", "two", nil),
	)
	out, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, 2, meta.MatchCount)
	require.Equal(t, 2, meta.ShownCount)
	// The footer is present in the tool-facing text.
	require.Contains(t, out, "-- 2 matched, 2 shown")
	// And the backward-compat text path strips it.
	compat := runSennitLogsText(path, SennitLogsParams{Limit: 10})
	require.NotContains(t, compat, "matched")
}

func TestSennitLogs_InvalidCursorIsAnError(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t, entryLine("INFO", "one", nil))
	_, _, err := runFull(t, path, SennitLogsParams{Cursor: "not-a-cursor", Limit: 10})
	require.Error(t, err)
	require.Contains(t, err.Error(), "cursor")
}

// --- block 3: match_count_exact / scanned_truncated ------------------------

// withScanBudget temporarily shrinks maxScanBytes so the scan-budget-exhausted
// path is exercisable without a multi-GB log, and restores it on cleanup. It
// must be called with t.Parallel() NOT in effect (it mutates package state), so
// tests using it run serially.
func withScanBudget(t *testing.T, b int64) {
	t.Helper()
	old := maxScanBytes
	maxScanBytes = b
	t.Cleanup(func() { maxScanBytes = old })
}

// TestSennitLogs_MatchCountExactWhenFullyScanned pins that when the scan walks
// the whole file, match_count_exact is true and scanned_truncated is false.
func TestSennitLogs_MatchCountExactWhenFullyScanned(t *testing.T) {
	t.Parallel()
	const total = 300
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("Entry %03d", i), nil))
	}
	path := writeRawLog(t, lines...)
	// A page of 100 from 300: the scan reaches the start (small file), so the
	// count is exact.
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, total, meta.MatchCount)
	require.True(t, meta.MatchCountExact, "a fully-scanned file must report an exact match count")
	require.False(t, meta.ScannedTruncated)
	require.True(t, meta.Truncated, "older matches exist, so truncated must be set")
}

// TestSennitLogs_ExactWhenBudgetEndsAtFileStart distinguishes exhaustion from
// truncation: consuming the final byte of a complete file is still a complete
// scan. An incomplete oversized line remains the separate caveat.
func TestSennitLogs_ExactWhenBudgetEndsAtFileStart(t *testing.T) {
	line := entryLine("INFO", "boundary", nil)
	path := writeRawLog(t, line)
	withScanBudget(t, int64(len(line)+1)) // include writeRawLog's newline

	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	require.True(t, meta.MatchCountExact)
	require.False(t, meta.ScannedTruncated)
	require.Equal(t, 1, meta.MatchCount)
}

// TestFileIdentityFallbackFailsClosed forces the no-stable-ID path. A same-size
// replacement with restored mtime must fail, while an append leaves the prefix
// and windows around the existing anchor unchanged.
// TestFileIdentityFallbackHashesCompletePrefix forces the no-stable-ID path.
// An append after the old file end must preserve a cursor near that end, while
// a replacement in the former unsampled middle of [0:offset] must be stale.
func TestFileIdentityFallbackHashesCompletePrefix(t *testing.T) {
	prefix := strings.Repeat("a", 1024)
	path := writeRawLog(t, prefix, "cursor-entry", "tail")
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	offset := int64(len(prefix) + 1) // cursor-entry starts after a >256-byte prefix
	noStableID := func(*os.File, os.FileInfo) (uint64, uint64) { return 0, 0 }
	identity, err := newFileIdentityWithStableID(f, info, offset, noStableID)
	require.NoError(t, err)
	require.Equal(t, offset, identity.prefixLength)

	// A short old tail followed by an append crosses the previous after-anchor
	// fingerprint window, but must not affect a prefix-only identity.
	appendLog(t, path, "appended")
	info, err = f.Stat()
	require.NoError(t, err)
	match, err := identity.matchesWithStableID(f, info, offset, noStableID)
	require.NoError(t, err)
	require.True(t, match)

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	contents[512] ^= 1 // middle of [0:offset], outside the old fixed windows
	require.NoError(t, os.WriteFile(path, contents, 0o600))
	info, err = f.Stat()
	require.NoError(t, err)
	match, err = identity.matchesWithStableID(f, info, offset, noStableID)
	require.NoError(t, err)
	require.False(t, match)
}

// TestSennitLogs_ScanBudgetTruncatesCount pins the scan-budget path: when the
// byte budget is exhausted before the scan reaches the start of the file,
// match_count is a LOWER bound, match_count_exact is false, and
// scanned_truncated is true. The budget is set tiny (via the seam) so the 8KB
// chunked scan stops almost immediately on a moderately sized file.
func TestSennitLogs_ScanBudgetTruncatesCount(t *testing.T) {
	// Serial: mutates the package-level maxScanBytes seam.
	const total = 400
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("Entry %03d", i), map[string]any{
			"pad": strings.Repeat("z", 40),
		}))
	}
	path := writeRawLog(t, lines...)
	// Set the budget below one chunk (8KB): the very first chunk read exhausts
	// it, so the scan cannot reach the start of the file.
	withScanBudget(t, 100)
	_, meta, err := runFull(t, path, SennitLogsParams{Limit: 10})
	require.NoError(t, err)
	require.False(t, meta.MatchCountExact, "a budget-capped scan must not claim an exact count")
	require.True(t, meta.ScannedTruncated, "a budget-capped scan must report scanned_truncated")
	// The count is a lower bound: it is whatever was seen before the stop, not
	// the true total.
	require.Less(t, meta.MatchCount, total, "a capped scan must under-report, not claim the total")
}

// --- block 6: schema contract + invalid-parameter text errors --------------

// TestSennitLogs_SchemaContract pins the generated input schema: the level
// parameter carries an enum of the four levels, and the description text for
// limit/lines/since states their ranges/validation. This is the contract that
// invalid values are rejected before the handler does real work (T9's
// "set enum in the schema, not only prose").
func TestSennitLogs_SchemaContract(t *testing.T) {
	t.Parallel()
	info := NewSennitLogsTool("unused.log").Info()
	// The fantasy schema is flat: Parameters maps each param name to its
	// property object (no nested "properties" wrapper).
	params := info.Parameters

	level, ok := params["level"].(map[string]any)
	require.True(t, ok, "the schema must have a level property")
	enum, ok := level["enum"].([]any)
	require.True(t, ok, "level must declare an enum in the schema")
	require.ElementsMatch(t, []any{"DEBUG", "INFO", "WARN", "ERROR"}, enum)

	// fantasy reflection does not derive numeric bounds from tags; the tool
	// wraps its ToolInfo so published schema consumers receive real bounds.
	for _, name := range []string{"limit", "lines"} {
		p, ok := params[name].(map[string]any)
		require.True(t, ok, "schema must have %s", name)
		require.Equal(t, 0, p["minimum"], "%s must permit explicit default", name)
		require.Equal(t, maxLogLines, p["maximum"], "%s must publish the safety cap", name)
	}
}

// TestSennitLogs_ValidationPrecedesFilesystemAccess ensures malformed input is
// rejected consistently for both missing and empty logs, before stat/open can
// turn it into a sentinel response.
func TestSennitLogs_ValidationPrecedesFilesystemAccess(t *testing.T) {
	empty := writeRawLog(t)
	missing := filepath.Join(t.TempDir(), "missing.log")
	invalid := []SennitLogsParams{
		{Limit: -1},
		{Lines: maxLogLines + 1},
		{Level: "verbose"},
		{Since: "yesterday"},
		{Cursor: "bad-cursor"},
		{Chain: true},
	}
	for _, path := range []string{empty, missing} {
		for _, params := range invalid {
			_, _, err := runFull(t, path, params)
			require.Error(t, err, "path=%s params=%+v", path, params)
		}
	}
}

// TestSennitLogs_InvalidLevelIsATextError pins that an unrecognized level is a
// text error (block 6), not a silent "any level" filter.
func TestSennitLogs_InvalidLevelIsATextError(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t, entryLine("INFO", "one", nil))
	_, _, err := runFull(t, path, SennitLogsParams{Level: "VERBOSE", Limit: 10})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid level")
}

// TestSennitLogs_InvalidSinceIsATextError pins that an unparseable since is a
// text error (block 6), not a silent "no time bound".
func TestSennitLogs_InvalidSinceIsATextError(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t, entryLine("INFO", "one", nil))
	_, _, err := runFull(t, path, SennitLogsParams{Since: "yesterday", Limit: 10})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid since")
}

// TestSennitLogs_InvalidLimitIsATextError pins that a limit above the cap is a
// text error (block 6), not a silent clamp.
func TestSennitLogs_InvalidLimitIsATextError(t *testing.T) {
	t.Parallel()
	path := writeRawLog(t, entryLine("INFO", "one", nil))
	for _, params := range []SennitLogsParams{
		{Limit: maxLogLines + 1},
		{Lines: maxLogLines + 1},
		{Limit: -1},
		{Lines: -1},
	} {
		_, _, err := runFull(t, path, params)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid")
	}
}

// TestSennitLogs_DefaultLimitIs100 pins the consistent default (block 6): a
// call with neither limit nor lines returns up to 100 entries.
func TestSennitLogs_DefaultLimitIs100(t *testing.T) {
	t.Parallel()
	const total = 150
	var lines []string
	for i := range total {
		lines = append(lines, entryLine("INFO", fmt.Sprintf("Entry %03d", i), nil))
	}
	path := writeRawLog(t, lines...)
	out, meta, err := runFull(t, path, SennitLogsParams{})
	require.NoError(t, err)
	require.Equal(t, 100, meta.ShownCount, "the default per-call limit must be 100")
	require.True(t, meta.Truncated, "150 entries > default 100, so the page is truncated")
	require.NotEmpty(t, stripMetaFooter(out))
}
