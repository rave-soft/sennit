package client_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/client"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/server"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// xdgIsolate redirects HOME and XDG_* to fresh temp dirs so config
// loading does not touch the host's real config, mirroring
// internal/workspace/multiclient_integration_test.go's helper of the
// same name (unexported to its package, so reimplemented here).
func xdgIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// newThreadsTestServer wires the production server handler around an
// httptest.NewServer, returning a client already connected to it.
func newThreadsTestServer(t *testing.T) *client.Client {
	t.Helper()
	xdgIsolate(t)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(hs.Close)

	u, err := url.Parse(hs.URL)
	require.NoError(t, err)
	c, err := client.NewClient(t.TempDir(), "tcp", u.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.RetireClient(context.Background()) })
	return c
}

// requireGitForThreadsTest and its helpers mirror
// internal/server/threads_test.go's git shell-out helpers (unexported
// to that package, so reimplemented here).
func requireGitForThreadsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForThreadsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// threadTestConfig is a project-local .braid.json (see
// internal/config.lookupConfigs) that gives the workspace a usable
// provider without any real credentials or network access: the model
// points at an unroutable address, so [thread.Manager]'s background
// agent dispatch fails fast with a connection error instead of the
// panic it hits with no provider configured at all (App.AgentCoordinator
// stays nil when config.IsConfigured() is false).
const threadTestConfig = `{
	"providers": {
		"test-provider": {
			"id": "test-provider",
			"type": "openai",
			"base_url": "http://127.0.0.1:1",
			"api_key": "test-key",
			"discover_models": false,
			"models": [
				{"id": "test-model", "context_window": 4096, "default_max_tokens": 100}
			]
		}
	},
	"models": {
		"large": {"provider": "test-provider", "model": "test-model"},
		"small": {"provider": "test-provider", "model": "test-model"}
	}
}`

// initThreadRepo creates a fresh git repository with one commit, so the
// server auto-attaches a thread manager to a workspace rooted at it (see
// thread.Attach, which requires the
// workspace path to be exactly the git toplevel).
func initThreadRepo(t *testing.T) string {
	t.Helper()
	requireGitForThreadsTest(t)

	dir := t.TempDir()
	runGitForThreadsTest(t, dir, "init", "-b", "main")
	runGitForThreadsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForThreadsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".braid.json"), []byte(threadTestConfig), 0o644))
	runGitForThreadsTest(t, dir, "add", "-A")
	runGitForThreadsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func TestThreads_CreateListGetSendMergeRemove(t *testing.T) {
	// initThreadRepo before newThreadsTestServer: t.Cleanup runs LIFO, so
	// registering the repo's t.TempDir cleanup first means it fires last —
	// after newThreadsTestServer's RetireClient/hs.Close have torn the
	// workspace's App down (including its agent coordinator's background
	// readiness work, see coordinator.Close). Reversed, the repo directory
	// would be removed while that work might still be touching it (e.g. a
	// `git status` subprocess), which is exactly the flake this ordering
	// avoids.
	repo := initThreadRepo(t)
	c := newThreadsTestServer(t)
	ctx := context.Background()

	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)

	threads, err := c.ListThreads(ctx, ws.ID)
	require.NoError(t, err)
	require.Empty(t, threads)

	created, err := c.CreateThread(ctx, ws.ID, proto.CreateThreadRequest{
		Name: "test-thread",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-thread", created.Name)
	require.Equal(t, "do the thing", created.Goal)

	got, err := c.GetThread(ctx, ws.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Goal, got.Goal)

	require.NoError(t, c.SendThread(ctx, ws.ID, created.ID, "keep going"))

	// The merge outcome (conflict/blocked/success) is recorded on the
	// thread's status, not returned as a Go error — only assert the
	// call round-trips a thread.
	merged, err := c.MergeThread(ctx, ws.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, merged.ID)

	// A thread that merged cleanly is already gone: the merge discards
	// it. Anything else still needs removing by hand, forced because the
	// outcome may have left it active.
	if merged.Status != string(thread.StatusMerged) {
		require.NoError(t, c.RemoveThread(ctx, ws.ID, created.ID, proto.RemoveThreadOptions{Force: true}))
	}

	_, err = c.GetThread(ctx, ws.ID, created.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, client.ErrNotFound))
}

func TestThreads_GetUnknownThreadReturnsNotFound(t *testing.T) {
	// See the ordering comment in TestThreads_CreateListGetSendMergeRemove:
	// the repo's t.TempDir cleanup must be registered before
	// newThreadsTestServer's, so it runs after workspace teardown.
	repo := initThreadRepo(t)
	c := newThreadsTestServer(t)
	ctx := context.Background()

	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)

	_, err = c.GetThread(ctx, ws.ID, "no-such-thread")
	require.Error(t, err)
	require.True(t, errors.Is(err, client.ErrNotFound))
}

func TestThreads_NonGitWorkspaceRejectsThreads(t *testing.T) {
	c := newThreadsTestServer(t)
	ctx := context.Background()

	// A bare temp dir with no git init: the server's workspace exists
	// but never gets a thread manager attached (see
	// thread.Attach), so thread
	// endpoints answer 409.
	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir()})
	require.NoError(t, err)

	_, err = c.ListThreads(ctx, ws.ID)
	require.ErrorIs(t, err, client.ErrThreadsUnsupported)
}
