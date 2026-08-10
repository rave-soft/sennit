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

// newStrandsTestServer wires the production server handler around an
// httptest.NewServer, returning a client already connected to it.
func newStrandsTestServer(t *testing.T) *client.Client {
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

// requireGitForStrandsTest and its helpers mirror
// internal/server/strands_test.go's git shell-out helpers (unexported
// to that package, so reimplemented here).
func requireGitForStrandsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForStrandsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

// strandTestConfig is a project-local .braid.json (see
// internal/config.lookupConfigs) that gives the workspace a usable
// provider without any real credentials or network access: the model
// points at an unroutable address, so [strand.Manager]'s background
// agent dispatch fails fast with a connection error instead of the
// panic it hits with no provider configured at all (App.AgentCoordinator
// stays nil when config.IsConfigured() is false).
const strandTestConfig = `{
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

// initStrandRepo creates a fresh git repository with one commit, so the
// server auto-attaches a strand manager to a workspace rooted at it (see
// internal/backend/strands.go's attachServerStrands, which requires the
// workspace path to be exactly the git toplevel).
func initStrandRepo(t *testing.T) string {
	t.Helper()
	requireGitForStrandsTest(t)

	dir := t.TempDir()
	runGitForStrandsTest(t, dir, "init", "-b", "main")
	runGitForStrandsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForStrandsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".braid.json"), []byte(strandTestConfig), 0o644))
	runGitForStrandsTest(t, dir, "add", "-A")
	runGitForStrandsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func TestStrands_CreateListGetSendMergeRemove(t *testing.T) {
	c := newStrandsTestServer(t)
	repo := initStrandRepo(t)
	ctx := context.Background()

	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)

	strands, err := c.ListStrands(ctx, ws.ID)
	require.NoError(t, err)
	require.Empty(t, strands)

	created, err := c.CreateStrand(ctx, ws.ID, proto.CreateStrandRequest{
		Name: "test-strand",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-strand", created.Name)
	require.Equal(t, "do the thing", created.Goal)

	got, err := c.GetStrand(ctx, ws.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Goal, got.Goal)

	require.NoError(t, c.SendStrand(ctx, ws.ID, created.ID, "keep going"))

	// The merge outcome (conflict/blocked/success) is recorded on the
	// strand's status, not returned as a Go error — only assert the
	// call round-trips a strand.
	merged, err := c.MergeStrand(ctx, ws.ID, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, merged.ID)

	// Force, since the merge outcome above may have left the strand
	// active rather than completed.
	require.NoError(t, c.RemoveStrand(ctx, ws.ID, created.ID, proto.RemoveStrandOptions{Force: true}))

	_, err = c.GetStrand(ctx, ws.ID, created.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, client.ErrNotFound))
}

func TestStrands_GetUnknownStrandReturnsNotFound(t *testing.T) {
	c := newStrandsTestServer(t)
	repo := initStrandRepo(t)
	ctx := context.Background()

	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: repo, DataDir: t.TempDir()})
	require.NoError(t, err)

	_, err = c.GetStrand(ctx, ws.ID, "no-such-strand")
	require.Error(t, err)
	require.True(t, errors.Is(err, client.ErrNotFound))
}

func TestStrands_NonGitWorkspaceRejectsStrands(t *testing.T) {
	c := newStrandsTestServer(t)
	ctx := context.Background()

	// A bare temp dir with no git init: the server's workspace exists
	// but never gets a strand manager attached (see
	// internal/backend/strands.go's attachServerStrands), so strand
	// endpoints answer 409.
	ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir()})
	require.NoError(t, err)

	_, err = c.ListStrands(ctx, ws.ID)
	require.Error(t, err)
}
