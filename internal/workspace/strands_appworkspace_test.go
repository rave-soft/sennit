package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/strand"
	"github.com/stretchr/testify/require"
)

// -- git test helpers (mirrors internal/strand/manager_test.go's style;
// unexported to that package, so reimplemented here). --

func requireGitForWorkspaceStrandsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForWorkspaceStrandsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func initRepoForWorkspaceStrandsTest(t *testing.T) string {
	t.Helper()
	requireGitForWorkspaceStrandsTest(t)

	dir := t.TempDir()
	runGitForWorkspaceStrandsTest(t, dir, "init", "-b", "main")
	runGitForWorkspaceStrandsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForWorkspaceStrandsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGitForWorkspaceStrandsTest(t, dir, "add", "-A")
	runGitForWorkspaceStrandsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func newTestStrandStoreDB(t *testing.T) strand.Store {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return strand.NewStore(db.New(conn))
}

// -- fakes: mirror internal/strand/manager_test.go's fakeSessions,
// fakeCoordinator, fakeHandle, fakeSpawner (unexported to package strand,
// so reimplemented here against the exported strand.Spawner/strand.Handle
// interfaces, the same approach internal/server/strands_test.go took). --

type fakeStrandSessions struct {
	session.Service
	mu sync.Mutex
	n  int
}

func (f *fakeStrandSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: "sess-" + title, Title: title}, nil
}

type fakeStrandCoordinator struct {
	agent.Coordinator
}

func (f *fakeStrandCoordinator) Run(_ context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeStrandCoordinator) CancelAll() {}

type fakeStrandHandle struct {
	id  string
	app *app.App
}

func (h *fakeStrandHandle) ID() string    { return h.id }
func (h *fakeStrandHandle) App() *app.App { return h.app }

// fakeStrandSpawner spawns a real (but network/db-free) app.App per call
// via app.NewForTest, wired with a fake Sessions/AgentCoordinator instead
// of the real ones a full bootstrap would build.
type fakeStrandSpawner struct {
	t *testing.T

	mu     sync.Mutex
	byPath map[string]*fakeStrandHandle
}

func newFakeStrandSpawner(t *testing.T) *fakeStrandSpawner {
	return &fakeStrandSpawner{t: t, byPath: make(map[string]*fakeStrandHandle)}
}

func (s *fakeStrandSpawner) Spawn(ctx context.Context, path string) (strand.Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeStrandSessions{}
	a.AgentCoordinator = &fakeStrandCoordinator{}

	h := &fakeStrandHandle{id: path, app: a}
	s.byPath[path] = h
	return h, nil
}

func (s *fakeStrandSpawner) Release(ctx context.Context, id string) error {
	return nil
}

// newTestStrandAppWorkspace wires an AppWorkspace whose App has a real
// *strand.Manager attached over a real git repo, a real store, and the
// fakeStrandSpawner defined above.
func newTestStrandAppWorkspace(t *testing.T) (*AppWorkspace, *strand.Manager) {
	t.Helper()
	repo := initRepoForWorkspaceStrandsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	mgr := strand.NewManager(strand.ManagerOptions{
		Store:       newTestStrandStoreDB(t),
		Spawner:     newFakeStrandSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetStrandManager(mgr)

	store := config.NewTestStore(&config.Config{}, repo)
	return NewAppWorkspace(a, store), mgr
}

func TestAppWorkspace_SupportsStrands(t *testing.T) {
	aw, _ := newTestStrandAppWorkspace(t)
	require.True(t, aw.SupportsStrands())

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	plain := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, t.TempDir()))
	require.False(t, plain.SupportsStrands())
}

func TestAppWorkspace_CreateListGetStrand(t *testing.T) {
	aw, _ := newTestStrandAppWorkspace(t)
	ctx := t.Context()

	strands, err := aw.ListStrands(ctx)
	require.NoError(t, err)
	require.Empty(t, strands)

	created, err := aw.CreateStrand(ctx, proto.CreateStrandRequest{
		Name: "test-strand",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-strand", created.Name)
	require.Equal(t, "do the thing", created.Goal)

	got, err := aw.GetStrand(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Goal, got.Goal)

	strands, err = aw.ListStrands(ctx)
	require.NoError(t, err)
	require.Len(t, strands, 1)
	require.Equal(t, created.ID, strands[0].ID)
}

func TestAppWorkspace_AttachStrand(t *testing.T) {
	aw, mgr := newTestStrandAppWorkspace(t)
	ctx := t.Context()

	created, err := aw.CreateStrand(ctx, proto.CreateStrandRequest{
		Name: "attach-me",
		Goal: "do the thing",
	})
	require.NoError(t, err)

	handle := mgr.Handle(created.ID)
	require.NotNil(t, handle, "strand's workspace should be live right after Create")

	attached, detach, err := aw.AttachStrand(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)

	attachedAW, ok := attached.(*AppWorkspace)
	require.True(t, ok)
	require.Same(t, handle.App(), attachedAW.App(), "AttachStrand must bind to the strand's own nested App")
	require.NotSame(t, aw.App(), attachedAW.App())

	require.NotPanics(t, func() { detach() })
}

func TestAppWorkspace_AttachStrand_UnknownID(t *testing.T) {
	aw, _ := newTestStrandAppWorkspace(t)

	_, _, err := aw.AttachStrand(t.Context(), "no-such-strand")
	require.Error(t, err)
}
