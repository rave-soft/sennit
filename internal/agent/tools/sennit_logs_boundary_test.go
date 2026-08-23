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
