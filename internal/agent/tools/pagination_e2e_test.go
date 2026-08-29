package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/rave-soft/sennit/internal/config"
)

func responseMetadata[T any](t *testing.T, responseMetadata string) T {
	t.Helper()
	var metadata T
	require.NoError(t, json.Unmarshal([]byte(responseMetadata), &metadata))
	return metadata
}

func grepResponseLines(content string) []string {
	matchLine := regexp.MustCompile(`^  Line ([0-9]+)(?:, Char [0-9]+)?:`)
	var lines []string
	for line := range strings.SplitSeq(content, "\n") {
		if match := matchLine.FindStringSubmatch(line); match != nil {
			lines = append(lines, match[1])
		}
	}
	return lines
}

func TestReadHandlerPaginationExactMetadataAndStableCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lines.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\n\nthree\nfour"), 0o600))
	tool := newReadToolForTest(dir)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "pagination-test")

	first := runReadTool(t, tool, ctx, ReadParams{FilePath: path, Limit: 2})
	firstMeta := responseMetadata[ReadResponseMetadata](t, first.Metadata)
	require.Equal(t, 4, firstMeta.TotalLines)
	require.Equal(t, 2, firstMeta.NextOffset)
	require.True(t, firstMeta.Truncated)
	require.Equal(t, "one\n", firstMeta.Content)
	require.NotEmpty(t, firstMeta.Cursor)

	second := runReadTool(t, tool, ctx, ReadParams{FilePath: path, Limit: 10, Cursor: firstMeta.Cursor})
	secondMeta := responseMetadata[ReadResponseMetadata](t, second.Metadata)
	require.Equal(t, "three\nfour", secondMeta.Content)
	require.Equal(t, 4, secondMeta.TotalLines)
	require.Zero(t, secondMeta.NextOffset)
	require.False(t, secondMeta.Truncated)

	mismatch := runReadTool(t, tool, ctx, ReadParams{FilePath: filepath.Join(dir, "other.txt"), Cursor: firstMeta.Cursor})
	require.True(t, mismatch.IsError)
}

func TestGrepHandlerPaginationSortContextAndGeneration(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "a.txt")
	newPath := filepath.Join(dir, "b.txt")
	require.NoError(t, os.WriteFile(oldPath, []byte("before\nneedle 1\n\nneedle 2\nafter\n"), 0o600))
	require.NoError(t, os.WriteFile(newPath, []byte("needle 3\nneedle 4\nneedle 5\n"), 0o600))
	now := time.Now()
	require.NoError(t, os.Chtimes(oldPath, now.Add(-time.Hour), now.Add(-time.Hour)))
	require.NoError(t, os.Chtimes(newPath, now, now))
	tool := NewGrepTool(dir, config.ToolGrep{})

	var got []string
	cursor := ""
	for pageSize := 1; ; pageSize++ {
		response := runToolWith(t, tool, t.Context(), GrepToolName, GrepParams{Pattern: "needle", Sort: "path", MaxResults: pageSize, Cursor: cursor, BeforeContext: 1, AfterContext: 1})
		require.False(t, response.IsError, response.Content)
		metadata := responseMetadata[GrepResponseMetadata](t, response.Metadata)
		require.Equal(t, 5, metadata.TotalMatches)
		got = append(got, grepResponseLines(response.Content)...)
		if cursor == "" {
			require.Contains(t, response.Content, "- Line 1: before")
			require.Contains(t, response.Content, "+ Line 3: ")
		}
		if !metadata.Truncated {
			break
		}
		cursor = metadata.Cursor
	}
	require.Equal(t, []string{"2", "4", "1", "2", "3"}, got)

	mtime := runToolWith(t, tool, t.Context(), GrepToolName, GrepParams{Pattern: "needle", Sort: "mtime", MaxResults: 2})
	require.False(t, mtime.IsError)
	require.Contains(t, strings.Split(mtime.Content, "\n")[1], "b.txt")
	mtimeMeta := responseMetadata[GrepResponseMetadata](t, mtime.Metadata)
	mismatch := runToolWith(t, tool, t.Context(), GrepToolName, GrepParams{Pattern: "other", Sort: "mtime", MaxResults: 2, Cursor: mtimeMeta.Cursor})
	require.True(t, mismatch.IsError)
	require.Contains(t, mismatch.Content, "does not match")
	require.NoError(t, os.WriteFile(newPath, []byte("needle changed\n"), 0o600))
	stale := runToolWith(t, tool, t.Context(), GrepToolName, GrepParams{Pattern: "needle", Sort: "mtime", MaxResults: 2, Cursor: mtimeMeta.Cursor})
	require.True(t, stale.IsError)
	require.Contains(t, stale.Content, "stale")
}

func TestRipgrepHandlerPaginationAndGoRegexDifference(t *testing.T) {
	dir := t.TempDir()
	fixtureDir := filepath.Join(dir, "fixtures")
	require.NoError(t, os.Mkdir(fixtureDir, 0o700))
	path := filepath.Join(fixtureDir, "data.txt")
	var content strings.Builder
	for i := range 230 {
		fmt.Fprintf(&content, " needle %03d\n", i)
	}
	require.NoError(t, os.WriteFile(path, []byte(content.String()), 0o600))
	command := func(ctx context.Context, pattern, searchPath, include string, caseInsensitive bool) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRipgrepFixtureHelper$", "--", pattern, searchPath, include, strconv.FormatBool(caseInsensitive))
	}
	tool := NewRipgrepTool(dir, config.ToolGrep{}, withRipgrepCommand(command))
	cursor := ""
	var got []string
	for _, size := range []int{37, 71, 100, 100} {
		response := runToolWith(t, tool, t.Context(), RipgrepToolName, RipgrepParams{Pattern: "needle", Path: "fixtures", Include: "*.txt", Sort: "path", MaxResults: size, Cursor: cursor})
		require.False(t, response.IsError, response.Content)
		metadata := responseMetadata[GrepResponseMetadata](t, response.Metadata)
		require.Equal(t, 230, metadata.TotalMatches)
		got = append(got, grepResponseLines(response.Content)...)
		cursor = metadata.Cursor
		if !metadata.Truncated {
			break
		}
	}
	require.Len(t, got, 230)
	for i, line := range got {
		require.Equal(t, strconv.Itoa(i+1), line)
	}

	goRegex := runToolWith(t, NewGrepTool(dir, config.ToolGrep{}), t.Context(), GrepToolName, GrepParams{Pattern: `\hneedle`})
	require.True(t, goRegex.IsError)
	rustRegex := runToolWith(t, tool, t.Context(), RipgrepToolName, RipgrepParams{Pattern: `\hneedle`, Path: "fixtures", Include: "*.txt", MaxResults: 1})
	require.False(t, rustRegex.IsError, rustRegex.Content)
	require.Contains(t, rustRegex.Content, "Line 1, Char 1")
}

func TestRipgrepFixtureHelper(t *testing.T) {
	separator := slices.Index(os.Args, "--")
	if separator < 0 {
		return
	}
	require.Len(t, os.Args[separator+1:], 4)
	pattern, searchPath, include, caseInsensitive := os.Args[separator+1], os.Args[separator+2], os.Args[separator+3], os.Args[separator+4]
	require.NotEmpty(t, pattern)
	require.Equal(t, "*.txt", include)
	ignoreCase, err := strconv.ParseBool(caseInsensitive)
	require.NoError(t, err)

	fixturePattern := strings.ReplaceAll(pattern, `\h`, `[\t ]`)
	if ignoreCase {
		fixturePattern = "(?i)" + fixturePattern
	}
	expression, err := regexp.Compile(fixturePattern)
	require.NoError(t, err)

	path := filepath.Join(searchPath, "data.txt")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	for lineNumber, line := range strings.Split(strings.TrimSuffix(string(content), "\n"), "\n") {
		match := expression.FindStringIndex(line)
		if match == nil {
			continue
		}
		record := ripgrepMatch{Type: "match"}
		record.Data.Path.Text = path
		record.Data.Lines.Text = line + "\n"
		record.Data.LineNumber = lineNumber + 1
		record.Data.Submatches = append(record.Data.Submatches, struct {
			Start int `json:"start"`
		}{Start: match[0]})
		require.NoError(t, json.NewEncoder(os.Stdout).Encode(record))
	}
	os.Exit(0)
}

func TestGlobAndLSHandlerPaginationNoGapsAndStaleGeneration(t *testing.T) {
	dir := t.TempDir()
	for i := range 215 {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-%03d.txt", i)), []byte("x"), 0o600))
	}
	globTool := NewGlobTool(dir, config.ToolGlob{})
	cursor := ""
	var files []string
	for _, size := range []int{23, 41, 73, 100} {
		response := runToolWith(t, globTool, t.Context(), GlobToolName, GlobParams{Pattern: "*.txt", MaxResults: size, Cursor: cursor})
		require.False(t, response.IsError, response.Content)
		metadata := responseMetadata[GlobResponseMetadata](t, response.Metadata)
		require.Equal(t, 215, metadata.TotalFiles)
		body := strings.Split(response.Content, "\n\n(")[0]
		files = append(files, strings.Split(body, "\n")...)
		cursor = metadata.Cursor
		if !metadata.Truncated {
			break
		}
	}
	require.Len(t, files, 215)
	// No gaps and no repeats is the property this test is named for. It
	// used to be checked by asserting the pages came back in path order,
	// which only held while the page key was the path; glob keys by
	// modification time, so completeness is checked directly instead.
	unique := map[string]bool{}
	for _, f := range files {
		require.False(t, unique[f], "%s appeared on two pages", f)
		unique[f] = true
	}
	require.Len(t, unique, 215)

	first := runToolWith(t, globTool, t.Context(), GlobToolName, GlobParams{Pattern: "*.txt", MaxResults: 10})
	firstMeta := responseMetadata[GlobResponseMetadata](t, first.Metadata)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file-new.txt"), []byte("x"), 0o600))
	stale := runToolWith(t, globTool, t.Context(), GlobToolName, GlobParams{Pattern: "*.txt", MaxResults: 10, Cursor: firstMeta.Cursor})
	require.True(t, stale.IsError)
	require.Contains(t, stale.Content, "stale")

	maxItems := 31
	lsTool := NewLsTool(nil, dir, config.ToolLs{MaxItems: &maxItems})
	cursor = ""
	totalSeen := 0
	for {
		response := runToolWith(t, lsTool, t.Context(), LSToolName, LSParams{Cursor: cursor})
		require.False(t, response.IsError, response.Content)
		metadata := responseMetadata[LSResponseMetadata](t, response.Metadata)
		require.Equal(t, 216, metadata.TotalFiles)
		totalSeen += metadata.NumberOfFiles
		cursor = metadata.Cursor
		if !metadata.Truncated {
			break
		}
	}
	require.Equal(t, 216, totalSeen)
}

func TestPaginationToolInfoSchemaContract(t *testing.T) {
	dir := t.TempDir()
	tools := []struct {
		name   string
		tool   fantasy.AgentTool
		bounds map[string][2]int
		sort   bool
	}{
		{name: "read", tool: newReadToolForTest(dir), bounds: map[string][2]int{"offset": {0, 0}, "limit": {0, DefaultReadLimit}}},
		{name: "grep", tool: NewGrepTool(dir, config.ToolGrep{}), bounds: map[string][2]int{"max_results": {0, maxPageResults}, "before_context": {0, 10}, "after_context": {0, 10}}, sort: true},
		{name: "ripgrep", tool: NewRipgrepTool(dir, config.ToolGrep{}), bounds: map[string][2]int{"max_results": {0, maxPageResults}, "before_context": {0, 10}, "after_context": {0, 10}}, sort: true},
		{name: "glob", tool: NewGlobTool(dir, config.ToolGlob{}), bounds: map[string][2]int{"max_results": {0, maxPageResults}}},
	}
	for _, test := range tools {
		t.Run(test.name, func(t *testing.T) {
			parameters := test.tool.Info().Parameters
			for name, bounds := range test.bounds {
				parameter, ok := parameters[name].(map[string]any)
				require.True(t, ok, "schema must contain %s", name)
				require.Equal(t, bounds[0], parameter["minimum"], "%s minimum", name)
				if bounds[1] > 0 {
					require.Equal(t, bounds[1], parameter["maximum"], "%s maximum", name)
				} else {
					require.NotContains(t, parameter, "maximum", "%s must not publish an artificial maximum", name)
				}
			}
			if test.sort {
				sortParameter, ok := parameters["sort"].(map[string]any)
				require.True(t, ok)
				require.ElementsMatch(t, []any{"path", "mtime"}, sortParameter["enum"])
			}
		})
	}
}

func TestRipgrepHandlerAcceptsLongJSONRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")
	line := "needle" + strings.Repeat("x", 320*1024)
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
	command := func(ctx context.Context, _, _, _ string, _ bool) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRipgrepLongRecordHelper$")
	}
	t.Setenv("SENNIT_RG_LONG_RECORD", path)
	tool := NewRipgrepTool(dir, config.ToolGrep{}, withRipgrepCommand(command))
	response := runToolWith(t, tool, t.Context(), RipgrepToolName, RipgrepParams{Pattern: "needle", Sort: "path", MaxResults: 1})
	require.False(t, response.IsError, response.Content)
	metadata := responseMetadata[GrepResponseMetadata](t, response.Metadata)
	require.Equal(t, 2, metadata.TotalMatches)
	require.Equal(t, 1, metadata.NumberOfMatches)
	require.True(t, metadata.Truncated)
	require.NotEmpty(t, metadata.Cursor)
	require.Less(t, len(response.Content), 10_000)
	require.Contains(t, response.Content, "...")

	second := runToolWith(t, tool, t.Context(), RipgrepToolName, RipgrepParams{Pattern: "needle", Sort: "path", MaxResults: 1, Cursor: metadata.Cursor})
	require.False(t, second.IsError, second.Content)
	secondMetadata := responseMetadata[GrepResponseMetadata](t, second.Metadata)
	require.Equal(t, 2, secondMetadata.TotalMatches)
	require.Equal(t, 1, secondMetadata.NumberOfMatches)
	require.False(t, secondMetadata.Truncated)
	require.Empty(t, secondMetadata.Cursor)
	require.Less(t, len(second.Content), 10_000)
	require.Contains(t, second.Content, "...")
}

func TestRipgrepLongRecordHelper(t *testing.T) {
	path := os.Getenv("SENNIT_RG_LONG_RECORD")
	if path == "" {
		return
	}
	line := "needle" + strings.Repeat("x", 320*1024) + "\n"
	for lineNumber := 1; lineNumber <= 2; lineNumber++ {
		record := ripgrepMatch{Type: "match"}
		record.Data.Path.Text = path
		record.Data.Lines.Text = line
		record.Data.LineNumber = lineNumber
		record.Data.Submatches = append(record.Data.Submatches, struct {
			Start int `json:"start"`
		}{Start: 0})
		require.NoError(t, json.NewEncoder(os.Stdout).Encode(record))
	}
	os.Exit(0)
}

func TestPaginationHandlerValidationBounds(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x"), 0o600))
	cases := []struct {
		name     string
		tool     fantasy.AgentTool
		toolName string
		params   any
	}{
		{name: "grep limit", tool: NewGrepTool(dir, config.ToolGrep{}), toolName: GrepToolName, params: GrepParams{Pattern: "x", MaxResults: 1001}},
		{name: "grep context", tool: NewGrepTool(dir, config.ToolGrep{}), toolName: GrepToolName, params: GrepParams{Pattern: "x", BeforeContext: 11}},
		{name: "glob limit", tool: NewGlobTool(dir, config.ToolGlob{}), toolName: GlobToolName, params: GlobParams{Pattern: "*", MaxResults: 1001}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			response := runToolWith(t, test.tool, t.Context(), test.toolName, test.params)
			require.True(t, response.IsError)
		})
	}
}
