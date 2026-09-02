package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestWebFetchToolNilPermissionsSkipsCheck(t *testing.T) {
	dir := t.TempDir()
	tool := NewWebFetchTool(nil, dir, dir, nil)

	resp, err := runWebFetchTool(t, tool, context.Background(), WebFetchParams{URL: ""})
	require.NoError(t, err)
	// No session ID in context and no permission service: the tool still
	// validates params directly instead of erroring on a missing session.
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "url is required")
}

func TestWebFetchToolDeniedPermission(t *testing.T) {
	perms := &stubPermissionService{granted: false}
	dir := t.TempDir()
	tool := NewWebFetchTool(perms, dir, dir, nil)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp, err := runWebFetchTool(t, tool, ctx, WebFetchParams{URL: "https://example.com"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.True(t, resp.StopTurn)
	require.Contains(t, resp.Content, "User denied permission")
}

// TestWebFetchToolLargePageWritesToPageDirNotWorkingDir proves a large
// fetched page lands in the tool's dedicated page directory rather than the
// user's working directory, which is what the permission request's Path
// names but must never receive a scratch file.
func TestWebFetchToolLargePageWritesToPageDirNotWorkingDir(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", LargeContentThreshold+1)))
	}))
	defer server.Close()

	workingDir := t.TempDir()
	pageDir := filepath.Join(t.TempDir(), "fetch")
	tool := NewWebFetchTool(nil, workingDir, pageDir, server.Client())

	resp, err := runWebFetchTool(t, tool, context.Background(), WebFetchParams{URL: server.URL})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "Content saved to: ")
	require.Contains(t, resp.Content, "Use the view and grep tools")

	matches, err := filepath.Glob(filepath.Join(pageDir, "page-*.md"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Contains(t, resp.Content, matches[0])

	workingDirMatches, err := filepath.Glob(filepath.Join(workingDir, "page-*.md"))
	require.NoError(t, err)
	require.Empty(t, workingDirMatches)
}

// TestWebFetchToolCreatesMissingPageDir proves the tool creates the page
// directory on demand — the production wiring points it at a subdirectory
// of the data directory that need not exist yet.
func TestWebFetchToolCreatesMissingPageDir(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("a", LargeContentThreshold+1)))
	}))
	defer server.Close()

	pageDir := filepath.Join(t.TempDir(), "does-not-exist-yet", "fetch")
	_, err := os.Stat(pageDir)
	require.True(t, os.IsNotExist(err))

	tool := NewWebFetchTool(nil, t.TempDir(), pageDir, server.Client())

	resp, err := runWebFetchTool(t, tool, context.Background(), WebFetchParams{URL: server.URL})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	matches, err := filepath.Glob(filepath.Join(pageDir, "page-*.md"))
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func runWebFetchTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params WebFetchParams) (fantasy.ToolResponse, error) {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  WebFetchToolName,
		Input: string(input),
	}

	return tool.Run(ctx, call)
}
