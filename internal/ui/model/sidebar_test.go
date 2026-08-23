package model

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestBackgroundJobsInfo(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	const width = 24
	info := backgroundJobsInfo(&sty, shell.BackgroundJobCounts{Active: 3, Completed: 7}, width)
	plain := ansi.Strip(info)

	require.Contains(t, plain, "Background Jobs")
	require.Contains(t, plain, "Active 3/50")
	require.NotContains(t, plain, "Completed")
	for line := range strings.Lines(info) {
		require.LessOrEqual(t, lipgloss.Width(strings.TrimSuffix(line, "\n")), width)
	}
}

// TestTruncatedMoreCount pins the "…and N more" arithmetic shared by the
// lsp/mcp/skills sidebar sections: truncating `total` items to maxItems
// shows maxItems-1 real entries plus one summary line, so N must cover
// everything past those maxItems-1 real entries, not past maxItems (an
// off-by-one that used to undercount by exactly one in lsp.go/mcp.go).
func TestTruncatedMoreCount(t *testing.T) {
	t.Parallel()

	// 10 items truncated to 5: 4 real rows + 1 summary row = 5 shown, so
	// the summary must account for the other 6, not 5.
	require.Equal(t, 6, truncatedMoreCount(10, 5))
	require.Equal(t, 2, truncatedMoreCount(4, 3))
}

// TestFileChangeCountAgreesWithFilesInfoFilter guards item 6: fileChangeCount
// (the sidebar summary line) must count exactly the files filesInfo's own
// filter (the "Modified Files" list body, session.go) would list, including
// ones that are uncommitted but have no diff stats yet — a file
// fileChangeCount used to skip because it looked only at Additions/
// Deletions and ignored Uncommitted.
func TestFileChangeCountAgreesWithFilesInfoFilter(t *testing.T) {
	t.Parallel()

	files := []SessionFile{
		{Additions: 1, Deletions: 0},                    // has a diff
		{Additions: 0, Deletions: 0},                    // no diff, committed: not a change
		{Additions: 0, Deletions: 0, Uncommitted: true}, // uncommitted, no diff yet: still a change
	}

	// filesInfo's own inclusion test (session.go), reproduced verbatim so
	// this test does not depend on fileChangeCount and filesInfo sharing an
	// implementation, only on agreeing which files count.
	var filesInfoCount int
	for _, f := range files {
		if f.Uncommitted || f.Additions != 0 || f.Deletions != 0 {
			filesInfoCount++
		}
	}

	require.Equal(t, filesInfoCount, fileChangeCount(files),
		"fileChangeCount (sidebar summary) must count the same files as the Modified Files list")
}
