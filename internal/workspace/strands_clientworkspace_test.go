package workspace_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// -- git test helpers (mirror internal/client/strands_test.go's
// requireGitForStrandsTest/runGitForStrandsTest/initStrandRepo; unexported
// to that package, so reimplemented here). --

func requireGitForClientWorkspaceStrandsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForClientWorkspaceStrandsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// strandTestConfigForClientWorkspace is a project-local .braid.json (see
// internal/client/strands_test.go's strandTestConfig) that gives the
// workspace a usable provider without any real credentials or network
// access, so strand.Manager's background agent dispatch fails fast
// instead of panicking with no provider configured.
const strandTestConfigForClientWorkspace = `{
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

// initStrandRepoForClientWorkspace creates a fresh git repository with one
// commit and a provider-configured .braid.json, so the server auto-attaches
// a strand manager to a workspace rooted at it.
func initStrandRepoForClientWorkspace(t *testing.T) string {
	t.Helper()
	requireGitForClientWorkspaceStrandsTest(t)

	dir := t.TempDir()
	runGitForClientWorkspaceStrandsTest(t, dir, "init", "-b", "main")
	runGitForClientWorkspaceStrandsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForClientWorkspaceStrandsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".braid.json"), []byte(strandTestConfigForClientWorkspace), 0o644))
	runGitForClientWorkspaceStrandsTest(t, dir, "add", "-A")
	runGitForClientWorkspaceStrandsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func TestClientWorkspace_SupportsStrands(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initStrandRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws := workspace.NewClientWorkspace(c, *wsProto)
	require.True(t, ws.SupportsStrands())

	nonGit := t.TempDir()
	c2 := rt.newClient(t, nonGit)
	wsProto2, err := c2.CreateWorkspace(ctx, proto.Workspace{Path: nonGit, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws2 := workspace.NewClientWorkspace(c2, *wsProto2)
	require.False(t, ws2.SupportsStrands())
}

func TestClientWorkspace_CreateListGetSendMergeRemove(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initStrandRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	ws := workspace.NewClientWorkspace(c, *wsProto)

	strands, err := ws.ListStrands(ctx)
	require.NoError(t, err)
	require.Empty(t, strands)

	created, err := ws.CreateStrand(ctx, proto.CreateStrandRequest{
		Name: "test-strand",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-strand", created.Name)

	got, err := ws.GetStrand(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)

	require.NoError(t, ws.SendStrand(ctx, created.ID, "keep going"))

	merged, err := ws.MergeStrand(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, merged.ID)

	require.NoError(t, ws.RemoveStrand(ctx, created.ID, proto.RemoveStrandOptions{Force: true}))

	_, err = ws.GetStrand(ctx, created.ID)
	require.Error(t, err)
}

func TestClientWorkspace_AttachStrandAndDetach(t *testing.T) {
	xdgIsolate(t)
	rt := newRuntimeServer(t)
	ctx := context.Background()

	repo := initStrandRepoForClientWorkspace(t)
	c := rt.newClient(t, repo)
	wsProto, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)
	parentWorkspaceID := wsProto.ID
	ws := workspace.NewClientWorkspace(c, *wsProto)

	created, err := ws.CreateStrand(ctx, proto.CreateStrandRequest{
		Name: "attach-me",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.WorkspaceID, "strand's workspace should be live right after Create")
	// Force-remove the strand during cleanup so its worktree/handle are
	// torn down before the repo's t.TempDir() is removed; AttachStrand's
	// detach deliberately leaves the strand itself running (see its doc
	// comment), so nothing else does this.
	t.Cleanup(func() {
		_ = ws.RemoveStrand(context.Background(), created.ID, proto.RemoveStrandOptions{Force: true})
	})

	attached, detach, err := ws.AttachStrand(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)

	// The attached Workspace is bound to the strand's own backend
	// workspace: a cheap call against it must succeed.
	_, err = attached.ListSessions(ctx)
	require.NoError(t, err)

	detach()

	// Detaching the strand view must not tear down the parent
	// ClientWorkspace's own claim — this is the whole point of the
	// derived-clientID mechanic in AttachStrand.
	_, err = c.GetWorkspace(ctx, parentWorkspaceID)
	require.NoError(t, err)
	_, err = ws.ListSessions(ctx)
	require.NoError(t, err)
}
