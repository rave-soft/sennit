package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNewRgSearchCmd_PatternPassedWithDashE is the regression test for a
// pattern beginning with "-" (e.g. "->") being parsed as a ripgrep flag
// instead of the search pattern: newRgSearchCmd used to append pattern as a
// bare positional argument, so a pattern like "->" became `rg ... -> path`,
// which ripgrep rejects with "unrecognized flag" (exit status 2). Passing
// it via "-e" makes ripgrep treat it as the pattern regardless of its
// leading characters.
func TestNewRgSearchCmd_PatternPassedWithDashE(t *testing.T) {
	t.Parallel()

	cmd := newRgSearchCmd(context.Background(), "rg", "->", "/some/path", "", false)
	args := cmd.Args[1:] // Args[0] is the binary name.

	idx := -1
	for i, a := range args {
		if a == "-e" {
			idx = i
			break
		}
	}
	require.NotEqual(t, -1, idx, "pattern must be passed via -e, not as a bare positional: %v", args)
	require.Less(t, idx+1, len(args))
	require.Equal(t, "->", args[idx+1], "the argument right after -e must be the pattern")

	// A dash-leading pattern must never appear as its own bare argument
	// (which ripgrep would parse as a flag).
	for i, a := range args {
		if a == "->" {
			require.Equal(t, "-e", args[i-1], "the pattern must only appear immediately after -e")
		}
	}
}
