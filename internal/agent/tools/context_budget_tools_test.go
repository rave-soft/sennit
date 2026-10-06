package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
)

// writeNeedleFile writes count matching lines of about 310 bytes each and
// returns the directory holding the file.
func writeNeedleFile(t *testing.T, count int) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, " needle %04d %s\n", i+1, strings.Repeat("x", 296))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.txt"), []byte(b.String()), 0o600))
	return dir
}

// pageThroughSearch follows a search tool's cursor to the end, giving every
// call a fresh step budget, and returns the line numbers it was shown, the
// number of pages, and the first page's text.
func pageThroughSearch(t *testing.T, tool fantasy.AgentTool, name string, freeTokens int64, params func(cursor string) any) (lines []string, pages int, first string) {
	t.Helper()
	cursor := ""
	for {
		ctx := t.Context()
		if freeTokens > 0 {
			ctx = WithContextBudget(ctx, NewContextBudget(freeTokens))
		}
		response := runToolWith(t, tool, ctx, name, params(cursor))
		require.False(t, response.IsError, response.Content)
		metadata := responseMetadata[GrepResponseMetadata](t, response.Metadata)
		body := pageBody(t, response.Content, metadata.Cursor)
		shown := grepResponseLines(body)
		require.Len(t, shown, metadata.NumberOfMatches)
		require.Contains(t, response.Content, fmt.Sprintf("Found %d matches\n", len(shown)))
		lines = append(lines, shown...)
		pages++
		if pages == 1 {
			first = body
		}
		require.Less(t, pages, 100, "the cursor is not advancing")
		if !metadata.Truncated {
			return lines, pages, first
		}
		require.NotEmpty(t, metadata.Cursor)
		cursor = metadata.Cursor
	}
}

func wantLineNumbers(count int) []string {
	want := make([]string, count)
	for i := range want {
		want[i] = strconv.Itoa(i + 1)
	}
	return want
}

// A page cut short by the context budget must hand back a cursor that
// resumes after the last match shown: every match once, none skipped.
func TestGrep_PagesByContextBudgetWithoutLosingMatches(t *testing.T) {
	t.Parallel()

	tool := NewGrepTool(nil, writeNeedleFile(t, 200), config.ToolGrep{})
	// 8k tokens free is 24KB of budget; a call is granted half, 12KB.
	lines, pages, first := pageThroughSearch(t, tool, GrepToolName, 8_000, func(cursor string) any {
		return GrepParams{Pattern: "needle", Sort: "path", MaxResults: 1000, Cursor: cursor}
	})
	require.Equal(t, wantLineNumbers(200), lines)
	require.Greater(t, pages, 3)
	require.LessOrEqual(t, len(first), 12*1024+200)
	require.Contains(t, first, "context window is nearly full")
}

// Without any budget the page is still bounded: result count times line
// width has no ceiling of its own.
func TestGrep_PageIsBoundedInBytesWithoutABudget(t *testing.T) {
	t.Parallel()

	tool := NewGrepTool(nil, writeNeedleFile(t, 700), config.ToolGrep{})
	lines, pages, first := pageThroughSearch(t, tool, GrepToolName, 0, func(cursor string) any {
		return GrepParams{Pattern: "needle", Sort: "path", MaxResults: 1000, Cursor: cursor}
	})
	require.Equal(t, wantLineNumbers(700), lines)
	require.Equal(t, 3, pages)
	require.LessOrEqual(t, len(first), maxGrepOutputBytes+200)
	require.Contains(t, first, "output size limit")
	require.NotContains(t, first, "context window")
}

// A default-sized page is what the cap must leave alone.
func TestGrep_DefaultPageIsNotCutByTheByteCap(t *testing.T) {
	t.Parallel()

	tool := NewGrepTool(nil, writeNeedleFile(t, 100), config.ToolGrep{})
	response := runToolWith(t, tool, t.Context(), GrepToolName, GrepParams{Pattern: "needle", Sort: "path"})
	metadata := responseMetadata[GrepResponseMetadata](t, response.Metadata)
	require.Equal(t, 100, metadata.NumberOfMatches)
	require.False(t, metadata.Truncated)
	require.NotContains(t, response.Content, "truncated")
}

// A match larger than the whole budget is still shown. A page with nothing
// on it would return a cursor to where it began.
func TestRenderGrepMatches_ShowsTheFirstMatchWhateverItsSize(t *testing.T) {
	t.Parallel()

	matches := []grepMatch{
		{path: "a.txt", lineNum: 1, lineText: strings.Repeat("x", 300)},
		{path: "a.txt", lineNum: 2, lineText: strings.Repeat("y", 300)},
	}
	out, shown, err := renderGrepMatchesWithContext(t.Context(), matches, false, 0, 0, 10, true)
	require.NoError(t, err)
	require.Equal(t, 1, shown)
	require.Contains(t, out, "Found 1 matches\n")
	require.Contains(t, out, "  Line 1: xxx")
	require.NotContains(t, out, "Line 2")
}

// With context lines one match can outweigh the whole budget. It is then
// shown as its own line, so the page neither stays empty nor overruns.
func TestGrep_FirstMatchDropsContextThatDoesNotFit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wide := strings.Repeat("c", 400)
	var b strings.Builder
	for range 30 {
		b.WriteString(wide + "\n")
	}
	b.WriteString("needle\n")
	for range 30 {
		b.WriteString(wide + "\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data.txt"), []byte(b.String()), 0o600))
	tool := NewGrepTool(nil, dir, config.ToolGrep{})
	params := GrepParams{Pattern: "needle", BeforeContext: 30, AfterContext: 30}

	roomy := runToolWith(t, tool, t.Context(), GrepToolName, params)
	require.Greater(t, len(roomy.Content), 24_000)
	require.NotContains(t, roomy.Content, "context lines omitted")

	// A full context is the minimum grant of 8KB; the match needs 24KB.
	tight := runToolWith(t, tool, WithContextBudget(t.Context(), NewContextBudget(-1)), GrepToolName, params)
	require.False(t, tight.IsError, tight.Content)
	require.Less(t, len(tight.Content), minContextBudgetBytes)
	require.Contains(t, tight.Content, "  Line 31, Char 1: needle\n")
	require.Contains(t, tight.Content, "context lines omitted")
	require.Equal(t, 1, responseMetadata[GrepResponseMetadata](t, tight.Metadata).NumberOfMatches)
}

func TestRipgrep_PagesByContextBudgetWithoutLosingMatches(t *testing.T) {
	t.Parallel()

	dir := writeNeedleFile(t, 60)
	command := func(ctx context.Context, pattern, searchPath, include string, caseInsensitive bool) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRipgrepFixtureHelper$", "--", pattern, searchPath, include, strconv.FormatBool(caseInsensitive))
	}
	tool := NewRipgrepTool(nil, dir, config.ToolGrep{}, withRipgrepCommand(command))
	lines, pages, first := pageThroughSearch(t, tool, RipgrepToolName, 8_000, func(cursor string) any {
		return RipgrepParams{Pattern: "needle", Include: "*.txt", Sort: "path", MaxResults: 1000, Cursor: cursor}
	})
	require.Equal(t, wantLineNumbers(60), lines)
	require.Equal(t, 2, pages)
	require.Contains(t, first, "context window is nearly full")
}

func TestTruncateOutputBytes(t *testing.T) {
	t.Parallel()

	short := "héllo"
	require.Equal(t, short, truncateOutputBytes(short, len(short), false))

	// Three-byte runes, so both cut points fall inside a character.
	content := "HEAD\n" + strings.Repeat("語\n", 5000) + "TAIL!"
	out := truncateOutputBytes(content, 1000, false)
	require.True(t, utf8.ValidString(out))
	require.LessOrEqual(t, len(out), 1000+60)
	require.True(t, strings.HasPrefix(out, "HEAD\n"))
	require.True(t, strings.HasSuffix(out, "TAIL!"))
	require.Regexp(t, `\.\.\. \[\d+ lines truncated\] \.\.\.`, out)

	require.Contains(t, truncateOutputBytes(content, 1000, true), "lines truncated: the context window is nearly full]")
}

// Escape sequences have no display width, so the width cap lets colored
// output through at many times its nominal size.
func TestFormatOutput_BoundsColoredOutputInBytes(t *testing.T) {
	t.Parallel()

	colored := strings.Repeat("\x1b[31mx\x1b[0m\n", 14_000) // 14k cells, 154KB per stream.
	require.Equal(t, colored, TruncateOutput(colored), "the width cap does not see this output as large")

	out := formatOutput(context.Background(), colored, colored, nil)
	require.LessOrEqual(t, len(out), maxBashOutputBytes+200)
	require.NotContains(t, out, "context window")
}

func TestFormatOutput_SharesTheContextBudgetBetweenStreams(t *testing.T) {
	t.Parallel()

	stdout := strings.Repeat(strings.Repeat("o", 99)+"\n", 250) // 25KB, under the width cap.
	// 4k tokens free is 12KB of budget; the call is granted 8KB, the
	// minimum, since half of it is less.
	out := formatOutput(budgetCtx(4_000), stdout, "boom: the reason it failed", nil)
	require.LessOrEqual(t, len(out), minContextBudgetBytes+200)
	require.Contains(t, out, "boom: the reason it failed")
	require.Contains(t, out, "the context window is nearly full")

	roomy := formatOutput(budgetCtx(180_000), stdout, "", nil)
	require.Equal(t, stdout, roomy)
}

func TestJobOutput_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	manager := shell.NewBackgroundShellManager()
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "test-session")
	line := strings.Repeat("j", 99)
	started := runBashTool(t, newBashToolWithManager(t.TempDir(), manager), ctx, BashParams{
		Description:     "print",
		Command:         "i=0; while [ $i -lt 250 ]; do echo " + line + "; i=$((i+1)); done; sleep 30",
		RunInBackground: true,
	})
	require.False(t, started.IsError, started.Content)
	var meta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(started.Metadata), &meta))
	tool := NewJobOutputTool(manager)
	require.Eventually(t, func() bool {
		return strings.Count(runBackgroundTool(t, tool, ctx, JobOutputToolName, meta.ShellID).Content, line) == 250
	}, 10*time.Second, 20*time.Millisecond)

	limited := runBackgroundTool(t, tool, WithContextBudget(ctx, NewContextBudget(4_000)), JobOutputToolName, meta.ShellID)
	require.LessOrEqual(t, len(limited.Content), minContextBudgetBytes+200)
	require.Contains(t, limited.Content, "the context window is nearly full")
}

func fetchWithBudget(t *testing.T, body string, ctx context.Context) fantasy.ToolResponse {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	tool := NewFetchTool(&stubPermissionService{granted: true}, t.TempDir(), nil)
	input, err := json.Marshal(FetchParams{URL: server.URL, Format: "text"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	return resp
}

func TestFetch_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	body := strings.Repeat("語", 20_000) // 60KB, under MaxFetchSize.
	resp := fetchWithBudget(t, body, budgetCtx(4_000))
	require.True(t, utf8.ValidString(resp.Content))
	require.LessOrEqual(t, len(resp.Content), minContextBudgetBytes+200)
	require.Contains(t, resp.Content, "the context window is nearly full]")

	require.Equal(t, body, fetchWithBudget(t, body, budgetCtx(180_000)).Content)
	require.Equal(t, body, fetchWithBudget(t, body, context.WithValue(t.Context(), SessionIDContextKey, "sess")).Content)
}

// A body cut at MaxFetchSize must still say so when the context budget
// cuts it again.
func TestFetch_KeepsTheSizeCapNoticeUnderAContextCut(t *testing.T) {
	t.Parallel()

	resp := fetchWithBudget(t, strings.Repeat("a", MaxFetchSize+10), budgetCtx(4_000))
	require.Contains(t, resp.Content, "the context window is nearly full]")
	require.True(t, strings.HasSuffix(resp.Content, fmt.Sprintf("[Content truncated to %d bytes]", MaxFetchSize)))
}

// A page that fits the usual inline threshold but not the context that is
// left goes to a file, like any page too large to inline.
func TestWebFetch_SavesToAFileWhatTheContextCannotHold(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", 20_000)))
	}))
	t.Cleanup(server.Close)
	pageDir := filepath.Join(t.TempDir(), "fetch")
	tool := NewWebFetchTool(nil, t.TempDir(), pageDir, server.Client())

	inline, err := runWebFetchTool(t, tool, context.Background(), WebFetchParams{URL: server.URL})
	require.NoError(t, err)
	require.NotContains(t, inline.Content, "Content saved to: ")
	require.Contains(t, inline.Content, strings.Repeat("a", 20_000))

	limited, err := runWebFetchTool(t, tool, WithContextBudget(context.Background(), NewContextBudget(4_000)), WebFetchParams{URL: server.URL})
	require.NoError(t, err)
	require.Contains(t, limited.Content, "Content saved to: ")
	matches, err := filepath.Glob(filepath.Join(pageDir, "page-*.md"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func TestFitContextBudget(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("語", 10_000) // 30KB.
	require.Equal(t, text, fitContextBudget(context.Background(), text))
	require.Equal(t, text, fitContextBudget(budgetCtx(180_000), text))

	cut := fitContextBudget(budgetCtx(4_000), text)
	require.True(t, utf8.ValidString(cut))
	require.LessOrEqual(t, len(cut), minContextBudgetBytes+100)
	require.Contains(t, cut, "the context window is nearly full]")
}

type textRunner struct{ text string }

func (r textRunner) RunTool(context.Context, mcp.ConfigProvider, string, string, string) (mcp.ToolResult, error) {
	return mcp.ToolResult{Type: "text", Content: r.text}, nil
}

// An MCP tool's result is capped only at megabytes by its own package.
func TestMCPTool_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("m", 300_000)
	tool := &Tool{
		mcpName: "srv",
		tool:    &mcp.Tool{Name: "dump"},
		cfg:     &stubMCPConfigProvider{cfg: &config.Config{}},
		reg:     textRunner{text: text},
	}
	run := func(ctx context.Context) fantasy.ToolResponse {
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "1", Name: tool.Name(), Input: "{}"})
		require.NoError(t, err)
		require.False(t, resp.IsError, resp.Content)
		return resp
	}
	require.Equal(t, text, run(context.WithValue(t.Context(), SessionIDContextKey, "sess")).Content)

	limited := run(budgetCtx(4_000))
	require.LessOrEqual(t, len(limited.Content), minContextBudgetBytes+100)
	require.Contains(t, limited.Content, "the context window is nearly full]")
}

func TestReadMCPResource_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	tool := NewReadMCPResourceTool(testConfigStore(t, t.TempDir()), fakeResourceReader{contents: []*mcp.ResourceContents{
		{URI: "res://doc", MIMEType: "text/plain", Text: strings.Repeat("r", 300_000)},
	}}, nil)
	resp, err := tool.Run(budgetCtx(4_000), fantasy.ToolCall{
		ID: "1", Name: ReadMCPResourceToolName,
		Input: mustJSONInput(t, ReadMCPResourceParams{MCPName: "srv", URI: "res://doc"}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.LessOrEqual(t, len(resp.Content), minContextBudgetBytes+100)
	require.Contains(t, resp.Content, "the context window is nearly full]")
}

func TestAgentOutput_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	manager := newFakeTaskManager()
	manager.tasks["t1"] = TaskInfo{ID: "t1", ParentSessionID: callerSession}
	manager.outputs["t1"] = TaskOutput{Messages: []TaskOutputMessage{{Role: "assistant", Text: strings.Repeat("o", 300_000)}}, Total: 1}
	ctx := WithContextBudget(context.WithValue(t.Context(), SessionIDContextKey, callerSession), NewContextBudget(4_000))

	resp, err := NewAgentOutputTool(manager, nil).Run(ctx, fantasy.ToolCall{ID: "1", Input: mustJSONInput(t, AgentOutputParams{ID: "t1"})})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.LessOrEqual(t, len(resp.Content), minContextBudgetBytes+100)
	require.Contains(t, resp.Content, "the context window is nearly full]")
}

func TestSennitLogs_IsHeldToTheContextBudget(t *testing.T) {
	t.Parallel()

	entries := make([]map[string]any, 100)
	for i := range entries {
		entries[i] = makeLogEntry("INFO", strings.Repeat("l", 1000), "app.go", i, nil)
	}
	tool := NewSennitLogsTool(createTestLogFile(t, entries))
	resp, err := tool.Run(budgetCtx(4_000), fantasy.ToolCall{ID: "1", Name: SennitLogsToolName, Input: mustJSONInput(t, SennitLogsParams{Lines: 100})})
	require.NoError(t, err)
	require.False(t, resp.IsError, resp.Content)
	require.LessOrEqual(t, len(resp.Content), minContextBudgetBytes+100)
	require.Contains(t, resp.Content, "the context window is nearly full]")
}

// A diff page is cut to the budget, and its cursor still walks the whole
// diff: the pages join into exactly what git printed.
func TestGitDiff_PagesByContextBudget(t *testing.T) {
	dir := gitToolRepo(t)
	path := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	gitToolCommand(t, dir, "add", "big.txt")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a changed line of some length\n", 800)), 0o644))
	tool := NewGitDiffTool(dir)

	params := GitDiffParams{}
	var all string
	pages := 0
	for {
		r, err := tool.Run(WithContextBudget(t.Context(), NewContextBudget(-1)), fantasy.ToolCall{ID: "test", Name: GitDiffToolName, Input: mustJSONInput(t, params)})
		require.NoError(t, err)
		require.False(t, r.IsError, r.Content)
		meta := responseMetadata[gitMeta](t, r.Metadata)
		body := pageBody(t, r.Content, meta.Cursor)
		require.LessOrEqual(t, len(body), minContextBudgetBytes)
		all += body
		pages++
		require.Less(t, pages, 50)
		if !meta.Truncated {
			break
		}
		params.Cursor = meta.Cursor
	}
	require.Equal(t, 4, pages)
	cmd := exec.CommandContext(t.Context(), "git", "-c", "core.quotepath=true", "diff", "--no-ext-diff", "--patch")
	cmd.Dir = dir
	want, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, string(want), all)
}
