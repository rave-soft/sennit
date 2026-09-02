package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/stretchr/testify/require"
)

// TestSymbolRangeEndLine is the regression test for eating one line too
// many (or, for add_after, inserting one line too late) when an LSP range
// ends at column 0: per protocol.Range's own doc comment, a range spanning
// through the end of line 11 (0-indexed) is reported as
// End:{Line:12, Character:0} - the start of the FOLLOWING line, not a
// position on line 12 itself.
func TestSymbolRangeEndLine(t *testing.T) {
	t.Parallel()

	t.Run("end at column 0 on a later line backs off to the true last line", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 0},
		}
		require.Equal(t, 11, symbolRangeEndLine(rng))
	})

	t.Run("end mid-line is unaffected", func(t *testing.T) {
		t.Parallel()
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 12, Character: 4},
		}
		require.Equal(t, 12, symbolRangeEndLine(rng))
	})

	t.Run("a single-line range ending at column 0 is not adjusted", func(t *testing.T) {
		t.Parallel()
		// Start == End.Line guards against a genuinely empty/zero-width
		// range being pushed to a negative line.
		rng := protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0},
			End:   protocol.Position{Line: 5, Character: 0},
		}
		require.Equal(t, 5, symbolRangeEndLine(rng))
	})
}

// TestReplaceSymbolThroughManagerRecordsOnlyTheEditedSpan is the regression
// test for defect 1: replace_symbol used to set wholeFileRead: true, so
// replacing one function retroactively marked the entire file as read for
// the session — after which edit/write would accept a blind change
// anywhere else in it. The fix records only the span replace_symbol
// actually touched, same as edit/multiedit/write already do for a partial
// change.
func TestReplaceSymbolThroughManagerRecordsOnlyTheEditedSpan(t *testing.T) {
	root := newLSPToolWorktree(t)
	manager := newLSPToolE2EManager(t, root, "replace-symbol")
	tracker := &mockEditFileTracker{}
	tool := NewReplaceSymbolTool(manager, &mockPermissionService{}, &mockHistoryService{}, tracker, root)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "replace-session")

	resp := runToolWith(t, tool, ctx, ReplaceSymbolToolName, ReplaceSymbolParams{
		Symbol:      "Exact",
		FilePath:    "a.go",
		Replacement: `func Exact() string { return "changed" }`,
	})
	require.False(t, resp.IsError, resp.Content)

	content, err := os.ReadFile(filepath.Join(root, "a.go"))
	require.NoError(t, err)
	require.Contains(t, string(content), "changed")

	// The tool never asked to record a whole-file read...
	require.Empty(t, tracker.reads)
	coverage := tracker.ReadCoverage(ctx, "replace-session", filepath.Join(root, "a.go"))
	require.False(t, coverage.Full, "replacing one symbol must not grant full-file coverage")
	// ...and coverage does not reach the untouched "Other" function three
	// lines below the one that was actually replaced.
	require.False(t, coverage.Covers(4, 4), "coverage must not reach lines the edit never touched")
	require.True(t, coverage.Covers(3, 3), "coverage must include the line that was actually replaced")
}
