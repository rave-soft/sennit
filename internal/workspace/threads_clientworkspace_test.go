package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// -- git test helpers (mirror internal/client/threads_test.go's
// requireGitForThreadsTest/runGitForThreadsTest/initThreadRepo; unexported
// to that package, so reimplemented here). --

func requireGitForClientWorkspaceThreadsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForClientWorkspaceThreadsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// threadTestConfigForClientWorkspace is a project-local .braid.json (see
// internal/client/threads_test.go's threadTestConfig) that gives the
// workspace a usable provider without any real credentials or network
// access, so thread.Manager's background agent dispatch fails fast
// instead of panicking with no provider configured.
const threadTestConfigForClientWorkspace = `{
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

// initThreadRepoForClientWorkspace creates a fresh git repository with one
// commit and a provider-configured .braid.json, so the server auto-attaches
// a thread manager to a workspace rooted at it.
func initThreadRepoForClientWorkspace(t *testing.T) string {
	t.Helper()
	requireGitForClientWorkspaceThreadsTest(t)

	dir := t.TempDir()
	runGitForClientWorkspaceThreadsTest(t, dir, "init", "-b", "main")
	runGitForClientWorkspaceThreadsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForClientWorkspaceThreadsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".braid.json"), []byte(threadTestConfigForClientWorkspace), 0o644))
	runGitForClientWorkspaceThreadsTest(t, dir, "add", "-A")
	runGitForClientWorkspaceThreadsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func TestClientWorkspace_SupportsThreads(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initThreadRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws := workspace.NewClientWorkspace(c, *wsProto)
	require.True(t, ws.SupportsThreads())

	nonGit := t.TempDir()
	c2 := rt.newClient(t, nonGit)
	wsProto2, err := c2.CreateWorkspace(ctx, proto.Workspace{Path: nonGit, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws2 := workspace.NewClientWorkspace(c2, *wsProto2)
	require.False(t, ws2.SupportsThreads())
}

func TestClientWorkspace_CreateListGetSendMergeRemove(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initThreadRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws := workspace.NewClientWorkspace(c, *wsProto)

	threads, err := ws.ListThreads(ctx)
	require.NoError(t, err)
	require.Empty(t, threads)

	created, err := ws.CreateThread(ctx, proto.CreateThreadRequest{
		Name: "test-thread",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-thread", created.Name)

	got, err := ws.GetThread(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)

	require.NoError(t, ws.SendThread(ctx, created.ID, "keep going"))

	merged, err := ws.MergeThread(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, merged.ID)

	require.NoError(t, ws.RemoveThread(ctx, created.ID, proto.RemoveThreadOptions{Force: true}))

	_, err = ws.GetThread(ctx, created.ID)
	require.Error(t, err)
}

func TestClientWorkspace_AttachThreadAndDetach(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initThreadRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	parentWorkspaceID := wsProto.ID
	ws := workspace.NewClientWorkspace(c, *wsProto)

	created, err := ws.CreateThread(ctx, proto.CreateThreadRequest{
		Name: "attach-me",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.WorkspaceID, "thread's workspace should be live right after Create")
	// Force-remove the thread during cleanup so its worktree/handle are
	// torn down before the repo's t.TempDir() is removed; AttachThread's
	// detach deliberately leaves the thread itself running (see its doc
	// comment), so nothing else does this.
	t.Cleanup(func() {
		_ = ws.RemoveThread(context.Background(), created.ID, proto.RemoveThreadOptions{Force: true})
	})

	attached, detach, err := ws.AttachThread(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)

	// The attached Workspace is bound to the thread's own backend
	// workspace: a cheap call against it must succeed.
	_, err = attached.ListSessions(ctx)
	require.NoError(t, err)

	detach()

	// Detaching the thread view must not tear down the parent
	// ClientWorkspace's own claim — this is the whole point of the
	// derived-clientID mechanic in AttachThread.
	_, err = c.GetWorkspace(ctx, parentWorkspaceID)
	require.NoError(t, err)
	_, err = ws.ListSessions(ctx)
	require.NoError(t, err)
}

func TestClientWorkspace_AttachThread_ClosedThread(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initThreadRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws := workspace.NewClientWorkspace(c, *wsProto)

	created, err := ws.CreateThread(ctx, proto.CreateThreadRequest{Name: "closed-thread", Goal: "do the thing"})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = ws.RemoveThread(context.Background(), created.ID, proto.RemoveThreadOptions{Force: true})
	})

	backendWorkspace, err := rt.srv.Backend().GetWorkspace(wsProto.ID)
	require.NoError(t, err)
	mgr, ok := backendWorkspace.ThreadManager().(*thread.Manager)
	require.True(t, ok)
	require.NoError(t, mgr.Shutdown(ctx))

	closed, err := ws.GetThread(ctx, created.ID)
	require.NoError(t, err)
	require.Empty(t, closed.WorkspaceID)
	require.Equal(t, created.SessionID, closed.SessionID)

	attached, detach, err := ws.AttachThread(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)
	require.True(t, workspace.IsReadOnlyError(attached.AgentRun(ctx, created.SessionID, "hello")))
	_, err = attached.GetSession(ctx, created.SessionID)
	require.NoError(t, err)
	_, err = attached.GetSession(ctx, "other-session")
	require.Error(t, err)

	attached.Shutdown()
	detach()
	_, err = ws.ListSessions(ctx)
	require.NoError(t, err)
}
