package tools

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
