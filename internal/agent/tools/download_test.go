package tools

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

// TestDownloadTool_AncestorSymlinkShowsResolvedTargetInPermissionDialog is
// the regression test for G21: download used to key and label the
// permission dialog on the unresolved file_path, so an ancestor directory
// symlink (`ln -s ../.. up` then `download <url> up/x`) showed a path that
// looked confined to the workspace while the write actually landed wherever
// the link pointed — two levels above it here.
func TestDownloadTool_AncestorSymlinkShowsResolvedTargetInPermissionDialog(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workdir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workdir, 0o755))
	outsideDir := filepath.Join(root, "outside")
	require.NoError(t, os.MkdirAll(outsideDir, 0o755))

	// "up" is a directory symlink inside the workspace pointing well
	// outside it; the requested file "up/x" never leaves the workspace's
	// string form, only its resolved location does.
	link := filepath.Join(workdir, "up")
	require.NoError(t, os.Symlink(outsideDir, link))
	wantResolved := filepath.Join(outsideDir, "x")

	perms := &recordingConfinedPermissions{confinedTestPermissions: &confinedTestPermissions{dir: ""}}
	tool := NewDownloadTool(perms, workdir, nil)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: "https://example.invalid/x", FilePath: "up/x"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "the request is denied, but a denial is still the tool's normal path here")
	require.Len(t, perms.requests, 1)
	require.Equal(t, wantResolved, perms.requests[0].Path,
		"the dialog must be keyed on where the download actually lands, not the unresolved request")
	require.Contains(t, perms.requests[0].Description, wantResolved,
		"the dialog must tell the user where the download actually resolves to")
}

// TestDownloadTool_RefusesRedirectFromApprovedHostToLoopbackAddress pins
// download's half of the redirect-guard fix (see fetch_test.go's
// TestFetchTool_RefusesRedirectFromApprovedHostToLinkLocalAddress for the
// full rationale): the user approves the URL they see, not wherever the
// server then redirects to, so a redirect that leaves the approved host for
// a loopback address must be refused rather than followed.
func TestDownloadTool_RefusesRedirectFromApprovedHostToLoopbackAddress(t *testing.T) {
	t.Parallel()

	// The redirect target must be a different host than the server below
	// (a loopback address, standing in for any approved host) — 169.254.
	// 169.254 is the classic cloud-metadata SSRF target, a link-local
	// address the redirect leaves the approved host for.
	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer entry.Close()

	workdir := t.TempDir()
	perms := &mockPermissionService{}
	tool := NewDownloadTool(perms, workdir, nil)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: entry.URL, FilePath: "out.txt"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "refusing to follow a redirect")

	_, err = os.Stat(filepath.Join(workdir, "out.txt"))
	require.True(t, os.IsNotExist(err), "a refused redirect must not create the output file")
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
func TestDownloadTool_RejectsOversizedContentLengthBeforeCreatingFile(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.FormatInt(int64(MaxDownloadSize+1), 10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	workdir := t.TempDir()
	destination := filepath.Join(workdir, "existing.txt")
	require.NoError(t, os.WriteFile(destination, []byte("keep this content"), 0o644))

	tool := NewDownloadTool(&mockPermissionService{}, workdir, nil)
	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: server.URL, FilePath: "existing.txt"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "maximum size")

	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "keep this content", string(content))
	entries, err := os.ReadDir(workdir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "an oversized Content-Length must not create a temporary file")
}

func TestDownloadTool_RejectsOversizedChunkedResponseAndCleansUp(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length makes net/http use chunked transfer encoding.
		chunk := make([]byte, 32*1024)
		for remaining := MaxDownloadSize + 1; remaining > 0; remaining -= len(chunk) {
			if remaining < len(chunk) {
				chunk = chunk[:remaining]
			}
			_, _ = w.Write(chunk)
		}
	}))
	t.Cleanup(server.Close)

	workdir := t.TempDir()
	destination := filepath.Join(workdir, "existing.txt")
	require.NoError(t, os.WriteFile(destination, []byte("keep this content"), 0o644))

	tool := NewDownloadTool(&mockPermissionService{}, workdir, nil)
	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  DownloadToolName,
		Input: mustJSONInput(t, DownloadParams{URL: server.URL, FilePath: "existing.txt"}),
	})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "maximum size")

	content, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Equal(t, "keep this content", string(content))
	entries, err := os.ReadDir(workdir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "temporary file must be removed after an oversized chunked response")
}

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
