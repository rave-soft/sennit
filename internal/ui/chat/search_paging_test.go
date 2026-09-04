package chat

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestGrepRenderTool_ReportsTotalAndIncomplete pins Audit 12 finding 1: a
// grep result's page size (NumberOfMatches), true total (TotalMatches) and
// whether part of the tree was left unread (Incomplete) are three
// different facts, and the transcript used to collapse them into one
// number — a 100-match page out of 4213 total looked identical to "that's
// everything". Both extra fields have to survive proto's copy of
// tools.GrepResponseMetadata and reach the rendered summary.
func TestGrepRenderTool_ReportsTotalAndIncomplete(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-grep",
		Name:     tools.GrepToolName,
		Input:    `{"pattern":"foo"}`,
		Finished: true,
	}
	meta, err := json.Marshal(tools.GrepResponseMetadata{
		NumberOfMatches: 100,
		TotalMatches:    4213,
		Truncated:       true,
		Incomplete:      true,
	})
	require.NoError(t, err)
	result := &message.ToolResult{Content: "some matches", Metadata: string(meta)}

	ctx := &GrepToolRenderContext{title: "Grep"}
	out := ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "100 of 4213 matches",
		"the summary must show the page size against the true total, not just the page size")
	require.Contains(t, out, "unreadable",
		"Incomplete must render as a distinct note from paging — a cursor cannot recover a part of the tree that was never walked")
}

// TestGlobRenderTool_ReportsTotalAndIncomplete is the glob twin of
// TestGrepRenderTool_ReportsTotalAndIncomplete.
func TestGlobRenderTool_ReportsTotalAndIncomplete(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-glob",
		Name:     tools.GlobToolName,
		Input:    `{"pattern":"*.go"}`,
		Finished: true,
	}
	meta, err := json.Marshal(tools.GlobResponseMetadata{
		NumberOfFiles: 10,
		TotalFiles:    500,
		Truncated:     true,
		Incomplete:    true,
	})
	require.NoError(t, err)
	result := &message.ToolResult{Content: "some files", Metadata: string(meta)}

	ctx := &GlobToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "10 of 500 files")
	require.Contains(t, out, "unreadable")
}

// TestLSRenderTool_ReportsTotalAndIncomplete is the ls twin.
func TestLSRenderTool_ReportsTotalAndIncomplete(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-ls",
		Name:     tools.LSToolName,
		Input:    `{"path":"."}`,
		Finished: true,
	}
	meta, err := json.Marshal(tools.LSResponseMetadata{
		NumberOfFiles: 3,
		TotalFiles:    9,
		Truncated:     true,
		Incomplete:    true,
	})
	require.NoError(t, err)
	result := &message.ToolResult{Content: "entries", Metadata: string(meta)}

	ctx := &LSToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "3 of 9 entries")
	require.Contains(t, out, "unreadable")
}

// TestReadRenderTool_ReportsTruncationAgainstTotalLines pins the read side
// of finding 1: a 20000-line file read with the default 2000-line limit
// used to summarize as "2000 lines" with no hint that it was a page, not
// the whole file. The summary must say how many lines the file actually
// has and that more can be read.
func TestReadRenderTool_ReportsTruncationAgainstTotalLines(t *testing.T) {
	t.Parallel()

	sty := styles.SennitDark()
	toolCall := message.ToolCall{
		ID:       "tc-read",
		Name:     tools.ReadToolName,
		Input:    `{"file_path":"big.go"}`,
		Finished: true,
	}
	content := ""
	for i := range 2000 {
		if i > 0 {
			content += "\n"
		}
		content += "line"
	}
	meta, err := json.Marshal(tools.ReadResponseMetadata{
		FilePath:   "big.go",
		Content:    content,
		TotalLines: 20000,
		NextOffset: 2000,
		Truncated:  true,
	})
	require.NoError(t, err)
	result := &message.ToolResult{Content: content, Metadata: string(meta)}

	ctx := &ReadToolRenderContext{}
	out := ansi.Strip(ctx.RenderTool(&sty, 120, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	}))

	require.Contains(t, out, "2000 of 20000 lines",
		"a read result must show the page against the file's true line count, not just the page size")
	require.Contains(t, out, "offset 2000",
		"Truncated must surface where a follow-up read would resume")
}
