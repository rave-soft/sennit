package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func gitToolCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.CommandContext(t.Context(), "git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}

func gitToolRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitToolCommand(t, dir, "init", "-b", "main")
	gitToolCommand(t, dir, "config", "user.email", "test@example.com")
	gitToolCommand(t, dir, "config", "user.name", "Test")
	return dir
}

func callGitTool[P any, M any](t *testing.T, tool fantasy.AgentTool, name string, p P) (fantasy.ToolResponse, M) {
	t.Helper()
	r, e := tool.Run(context.Background(), fantasy.ToolCall{ID: "test", Name: name, Input: mustJSONInput(t, p)})
	require.NoError(t, e)
	var m M
	if !r.IsError {
		require.NoError(t, json.Unmarshal([]byte(r.Metadata), &m))
	}
	return r, m
}

func TestGitStatusStructuredRenameAndPagination(t *testing.T) {
	dir := gitToolRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "old"), []byte("x"), 0o644))
	gitToolCommand(t, dir, "add", "old")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	gitToolCommand(t, dir, "mv", "old", "new")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o644))
	tool := NewGitStatusTool(dir)
	_, first := callGitTool[GitStatusParams, gitMeta](t, tool, GitStatusToolName, GitStatusParams{Limit: 1, IncludeUntracked: true})
	require.Len(t, first.Entries.([]any), 1)
	require.True(t, first.Truncated)
	_, second := callGitTool[GitStatusParams, gitMeta](t, tool, GitStatusToolName, GitStatusParams{Limit: 1, IncludeUntracked: true, Cursor: first.Cursor})
	require.False(t, second.Truncated)
	require.NotEmpty(t, second.Entries)
}

func TestGitDiffPaginationRejectsMutation(t *testing.T) {
	dir := gitToolRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("before\n"), 0o644))
	gitToolCommand(t, dir, "add", "a")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("after\n"+string(make([]byte, 400))), 0o644))
	tool := NewGitDiffTool(dir)
	_, first := callGitTool[GitDiffParams, gitMeta](t, tool, GitDiffToolName, GitDiffParams{MaxBytes: 20})
	require.True(t, first.Truncated)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), []byte("changed again\n"), 0o644))
	r, _ := callGitTool[GitDiffParams, gitMeta](t, tool, GitDiffToolName, GitDiffParams{MaxBytes: 20, Cursor: first.Cursor})
	require.True(t, r.IsError)
	require.Contains(t, r.Content, "stale cursor")
}

func TestGitDiffDisablesRepositoryTextconv(t *testing.T) {
	dir := gitToolRepo(t)
	path := filepath.Join(dir, "secret.marker")
	require.NoError(t, os.WriteFile(path, []byte("before\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.marker diff=marker\n"), 0o644))
	gitToolCommand(t, dir, "add", ".gitattributes", "secret.marker")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	gitToolCommand(t, dir, "config", "diff.marker.textconv", "printf TEXTCONV_EXECUTED")
	require.NoError(t, os.WriteFile(path, []byte("after\n"), 0o644))

	r, _ := callGitTool[GitDiffParams, gitMeta](t, NewGitDiffTool(dir), GitDiffToolName, GitDiffParams{Format: "patch"})
	require.False(t, r.IsError)
	require.NotContains(t, r.Content, "TEXTCONV_EXECUTED")
	require.Contains(t, r.Content, "after")
}

func TestGitDiffStreamsOverLegacyOutputCap(t *testing.T) {
	// The legacy cap is lowered for the duration of the test rather than
	// out-writing the real 10MB one. What the test is about is that the
	// streaming path is not bound by the cap at all, which a diff of any
	// size past it demonstrates equally well — and a 10MB fixture, diffed
	// and streamed through race-instrumented code, took 26 seconds, more
	// than half of this package's whole race run.
	oldCap := gitOutputCap
	gitOutputCap = 64 << 10
	t.Cleanup(func() { gitOutputCap = oldCap })

	dir := gitToolRepo(t)
	path := filepath.Join(dir, "large.txt")
	require.NoError(t, os.WriteFile(path, []byte("base\n"), 0o644))
	gitToolCommand(t, dir, "add", "large.txt")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	// Line-oriented text ensures Git emits a patch rather than a binary summary.
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("changed line substantially longer than the original\n", 6000)), 0o644))
	r, meta := callGitTool[GitDiffParams, gitMeta](t, NewGitDiffTool(dir), GitDiffToolName, GitDiffParams{MaxBytes: 200000})
	require.False(t, r.IsError)
	require.Greater(t, meta.TotalBytes, gitOutputCap)
	require.True(t, meta.Truncated)
}

func TestGitDiffUTF8PagesJoinExactly(t *testing.T) {
	dir := gitToolRepo(t)
	path := filepath.Join(dir, "unicode.txt")
	require.NoError(t, os.WriteFile(path, []byte("old\n"), 0o644))
	gitToolCommand(t, dir, "add", "unicode.txt")
	gitToolCommand(t, dir, "commit", "-m", "initial")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("é日🙂\n", 40)), 0o644))
	tool := NewGitDiffTool(dir)
	params := GitDiffParams{MaxBytes: 19}
	var all string
	for {
		r, meta := callGitTool[GitDiffParams, gitMeta](t, tool, GitDiffToolName, params)
		require.False(t, r.IsError)
		require.True(t, utf8.ValidString(r.Content))
		require.LessOrEqual(t, len(r.Content), params.MaxBytes)
		all += r.Content
		if !meta.Truncated {
			break
		}
		params.Cursor = meta.Cursor
	}
	cmd := exec.CommandContext(t.Context(), "git", "-c", "core.quotepath=true", "diff", "--no-ext-diff", "--patch")
	cmd.Dir = dir
	expected, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, string(expected), all)
}

func TestParseNumstatRenameAndByteBudget(t *testing.T) {
	entries := parseNumstat([]byte("0\t0\t\x00old name\x00new name\x001\t2\tplain\x00"))
	require.Equal(t, []gitDiffStatEntry{{Path: "new name", OriginalPath: "old name", Added: "0", Deleted: "0"}, {Path: "plain", Added: "1", Deleted: "2"}}, entries)
	page, _, rendered, more, err := pageStat(entries, "", len(statLine(entries[0])))
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, len(renderStat(page)), rendered)
	require.True(t, more)
	_, _, _, _, err = pageStat(entries, "", 1)
	require.Error(t, err)
}

func TestGitLogEmptyRepositoryAndPathEscape(t *testing.T) {
	dir := gitToolRepo(t)
	r, m := callGitTool[GitLogParams, gitMeta](t, NewGitLogTool(dir), GitLogToolName, GitLogParams{})
	require.False(t, r.IsError)
	require.Equal(t, 0, m.Total)
	r, _ = callGitTool[GitStatusParams, gitMeta](t, NewGitStatusTool(dir), GitStatusToolName, GitStatusParams{Paths: []string{"../outside"}})
	require.True(t, r.IsError)
}

func TestGitSpoolHelper(t *testing.T) {
	if os.Getenv("GIT_SPOOL_HELPER") == "" {
		return
	}
	if os.Getenv("GIT_SPOOL_HELPER") == "forever" {
		for {
			_, _ = os.Stdout.Write([]byte("continuing output\n"))
		}
	}
	for i := 0; i < 300000; i++ {
		_, _ = fmt.Fprintf(os.Stdout, "1\t0\tpath-%08d-long-enough-for-streaming\x00", i)
	}
	os.Exit(0)
}

func TestSpoolStopsWriterAndRemovesTempFile(t *testing.T) {
	dir := gitToolRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)
	oldCommand, oldCap := gitCommandContext, gitSpoolCap
	gitCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitSpoolHelper$")
	}
	gitSpoolCap = 64
	t.Cleanup(func() { gitCommandContext, gitSpoolCap = oldCommand, oldCap })
	t.Setenv("GIT_SPOOL_HELPER", "forever")
	done := make(chan error, 1)
	go func() { _, _, _, err := spoolGitOutput(context.Background(), dir, true, "diff"); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("spool deadlocked while child continued writing")
	}
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSpoolStatOverLegacyOutputCapAndRemovesTempFile(t *testing.T) {
	dir := gitToolRepo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMP", tmp)
	oldCommand, oldCap := gitCommandContext, gitSpoolCap
	gitCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGitSpoolHelper$")
	}
	gitSpoolCap = defaultGitSpoolCap
	t.Cleanup(func() { gitCommandContext, gitSpoolCap = oldCommand, oldCap })
	t.Setenv("GIT_SPOOL_HELPER", "stat")
	file, total, _, err := spoolGitOutput(context.Background(), dir, false, "diff", "--numstat", "-z")
	require.NoError(t, err)
	require.Greater(t, total, gitOutputCap)
	page, _, _, more, count, err := pageStatFile(file, "", 200000)
	require.NoError(t, err)
	require.NotEmpty(t, page)
	require.Less(t, len(page), count)
	require.True(t, more)
	require.Equal(t, 300000, count)
	closeAndRemove(file)
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries)
}
