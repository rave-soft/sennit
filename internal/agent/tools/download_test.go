package tools

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestDownloadTool_DoesNotFollowSymlinkOutOfWorkspace pins DEFECT 2:
// os.Create(filePath) follows an existing symlink and truncates whatever it
// points at, so a link planted at the download's destination (pre-existing,
// or created by the bash tool) let a download clobber a file outside the
// workspace the moment the response body started arriving — even before
// this test's assertion that the copy completed. The fix writes to a temp
// file next to the destination and renames it into place, which never
// follows a link at filePath.
func TestDownloadTool_DoesNotFollowSymlinkOutOfWorkspace(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("downloaded content"))
	}))
	defer server.Close()

	root := t.TempDir()
	workdir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outsideDir := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	target := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(target, []byte("do not touch"), 0o644))

	link := filepath.Join(workdir, "cfg")
	require.NoError(t, os.Symlink(target, link))

	perms := &mockPermissionService{}
	tool := NewDownloadTool(perms, workdir, nil)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: server.URL, FilePath: link}),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "unconfined download should proceed: %v", resp)

	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "do not touch", string(onDisk), "the symlink target outside the workspace must be untouched")

	// The rename replaces the symlink's own directory entry with the
	// downloaded content, the same way it would replace a plain file at
	// that path — it just never follows the link to get there.
	downloaded, err := os.ReadFile(link)
	require.NoError(t, err)
	require.Equal(t, "downloaded content", string(downloaded))
}

// TestDownloadTool_DefaultClientDoesNotCapBelowCallerTimeout is download's
// half of DEFECT 1 (see fetch_test.go's
// TestFetchTool_DefaultClientDoesNotCapBelowCallerTimeout for the full
// rationale): NewDownloadTool used to give its default client a fixed 5
// minute http.Client.Timeout on top of the per-call context timeout
// derived from the "timeout" parameter, so that fixed value - not the
// caller's - was the real ceiling. A tool built with an explicit
// low-Timeout client stands in for that old capped default and must still
// get cut short; a tool built with client: nil - the actual production
// default-construction path - must not.
func TestDownloadTool_DefaultClientDoesNotCapBelowCallerTimeout(t *testing.T) {
	t.Parallel()

	const slowServerDelay = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(slowServerDelay)
		_, _ = w.Write([]byte("downloaded content"))
	}))
	t.Cleanup(server.Close)

	perms := &mockPermissionService{}
	// requested timeout (2s) comfortably exceeds both the slow server's
	// delay and the stand-in "old cap" (100ms) below, so a failure here can
	// only come from that fixed client-level cap, not from the request
	// simply running long.
	params := DownloadParams{URL: server.URL, FilePath: "out.txt", Timeout: 2}
	input := mustJSONInput(t, params)

	cappedClient := &http.Client{Timeout: 100 * time.Millisecond}
	capped := NewDownloadTool(perms, t.TempDir(), cappedClient)
	resp, err := capped.Run(confinedTestCtx(t), fantasy.ToolCall{ID: "call-1", Input: input})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a client-level Timeout below the caller's requested timeout must still cut the request short")

	uncapped := NewDownloadTool(perms, t.TempDir(), nil)
	resp, err = uncapped.Run(confinedTestCtx(t), fantasy.ToolCall{ID: "call-2", Input: input})
	require.NoError(t, err)
	require.False(t, resp.IsError, "NewDownloadTool's default client (client: nil) must not impose a fixed cap below the caller's timeout")
}
