package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// writeNumberedFile writes lines of 100 bytes each, so a byte budget maps
// onto a line count a test can state.
func writeNumberedFile(t *testing.T, lines int) string {
	t.Helper()
	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, "%05d %s\n", i+1, strings.Repeat("x", 93))
	}
	path := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	return path
}

func budgetCtx(freeTokens int64) context.Context {
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess")
	return WithContextBudget(ctx, NewContextBudget(freeTokens))
}

func TestReserveContextBudget_NoBudgetGrantsEverything(t *testing.T) {
	t.Parallel()

	granted, limited := reserveContextBudget(context.Background(), MaxReadSize)
	require.Equal(t, MaxReadSize, granted)
	require.False(t, limited)
}

// The calls of one step are dispatched without knowing how many there are,
// so whatever their number they must not add up to more than the budget.
func TestReserveContextBudget_SiblingCallsStayWithinTheBudget(t *testing.T) {
	t.Parallel()

	const freeTokens = 40_000
	ctx := budgetCtx(freeTokens)
	total := 0
	for range 4 {
		granted, limited := reserveContextBudget(ctx, MaxReadSize)
		require.True(t, limited)
		total += granted
	}
	require.LessOrEqual(t, total, freeTokens*contextBudgetBytesPerToken)
}

func TestReserveContextBudget_ReleaseReturnsWhatWasNotUsed(t *testing.T) {
	t.Parallel()

	ctx := budgetCtx(40_000)
	first, _ := reserveContextBudget(ctx, MaxReadSize)
	releaseContextBudget(ctx, first)
	second, _ := reserveContextBudget(ctx, MaxReadSize)
	require.Equal(t, first, second)
}

// A context already past its limit still gets a read large enough to hold
// a line: an empty result with a cursor to its own start is a loop.
func TestReserveContextBudget_FullContextStillGrantsTheMinimum(t *testing.T) {
	t.Parallel()

	ctx := budgetCtx(-5_000)
	for range 3 {
		granted, limited := reserveContextBudget(ctx, MaxReadSize)
		require.Equal(t, minContextBudgetBytes, granted)
		require.True(t, limited)
	}
	require.Greater(t, minContextBudgetBytes, MaxLineLength+len("..."))
}

func TestRead_StopsAtTheContextBudgetAndSaysSo(t *testing.T) {
	t.Parallel()

	path := writeNumberedFile(t, 1500) // 150KB, under MaxReadSize.
	tool := newReadToolForTest(filepath.Dir(path))

	// 20k tokens free is 60KB of budget; one call gets half, 30KB.
	resp := runReadTool(t, tool, budgetCtx(20_000), ReadParams{FilePath: path})
	require.False(t, resp.IsError)
	var meta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.True(t, meta.Truncated)
	require.LessOrEqual(t, len(meta.Content), 30_000)
	require.Greater(t, meta.NextOffset, 250)
	require.Contains(t, resp.Content, fmt.Sprintf("Showing lines 1-%d of 1500.", meta.NextOffset))
	require.Contains(t, resp.Content, "context window is nearly full")
	require.Contains(t, resp.Content, fmt.Sprintf("'offset' %d", meta.NextOffset))
}

func TestRead_RoomyContextReadsTheWholeFile(t *testing.T) {
	t.Parallel()

	path := writeNumberedFile(t, 1500)
	tool := newReadToolForTest(filepath.Dir(path))

	resp := runReadTool(t, tool, budgetCtx(180_000), ReadParams{FilePath: path})
	var meta ReadResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.False(t, meta.Truncated)
	require.NotContains(t, resp.Content, "Showing lines")
}

// A read the caller's own limit cut short is not the context's doing, and
// must not be reported as such.
func TestRead_LineLimitIsNotBlamedOnTheContext(t *testing.T) {
	t.Parallel()

	path := writeNumberedFile(t, 1500)
	tool := newReadToolForTest(filepath.Dir(path))

	resp := runReadTool(t, tool, budgetCtx(20_000), ReadParams{FilePath: path, Limit: 50})
	require.Contains(t, resp.Content, "Showing lines 1-50 of 1500.")
	require.NotContains(t, resp.Content, "context window")
}

// A read returns what it did not use, so a small file read first does not
// shrink the read that follows it in the same step.
func TestRead_ReturnsUnusedBudgetToTheStep(t *testing.T) {
	t.Parallel()

	small := writeNumberedFile(t, 10)
	tool := newReadToolForTest(filepath.Dir(small))
	ctx := budgetCtx(20_000)

	before, _ := reserveContextBudget(ctx, MaxReadSize)
	releaseContextBudget(ctx, before)
	runReadTool(t, tool, ctx, ReadParams{FilePath: small})
	after, _ := reserveContextBudget(ctx, MaxReadSize)
	require.InDelta(t, before, after, 1000)
}

func TestMultiRead_StopsAtTheContextBudget(t *testing.T) {
	t.Parallel()

	path := writeNumberedFile(t, 1500)
	tool := newMultiReadToolForTest(filepath.Dir(path))
	input, err := json.Marshal(MultiReadParams{Files: []MultiReadItem{{FilePath: path}}})
	require.NoError(t, err)

	resp, err := tool.Run(budgetCtx(20_000), fantasy.ToolCall{ID: "call", Name: MultiReadToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	var out MultiReadResponse
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &out))
	require.True(t, out.Truncated)
	require.LessOrEqual(t, out.Bytes, 30_000)
	require.NotEmpty(t, out.Cursor)
}
