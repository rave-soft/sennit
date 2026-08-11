package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// -- git test helpers (mirrors internal/thread/manager_test.go's style;
// unexported to that package, so reimplemented here). --

func requireGitForWorkspaceThreadsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForWorkspaceThreadsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func initRepoForWorkspaceThreadsTest(t *testing.T) string {
	t.Helper()
	requireGitForWorkspaceThreadsTest(t)

	dir := t.TempDir()
	runGitForWorkspaceThreadsTest(t, dir, "init", "-b", "main")
	runGitForWorkspaceThreadsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForWorkspaceThreadsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGitForWorkspaceThreadsTest(t, dir, "add", "-A")
	runGitForWorkspaceThreadsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

func newTestThreadStoreDB(t *testing.T) thread.Store {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return thread.NewStore(db.New(conn), dataDir)
}

// -- fakes: mirror internal/thread/manager_test.go's fakeSessions,
// fakeCoordinator, fakeHandle, fakeSpawner (unexported to package thread,
// so reimplemented here against the exported thread.Spawner/thread.Handle
// interfaces, the same approach internal/server/threads_test.go took). --

type fakeThreadSessions struct {
	session.Service
	mu sync.Mutex
	n  int
}

func (f *fakeThreadSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: "sess-" + title, Title: title}, nil
}

type fakeThreadCoordinator struct {
	agent.Coordinator
}

func (f *fakeThreadCoordinator) Run(_ context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeThreadCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *fakeThreadCoordinator) RunAccepted(_ context.Context, _ *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return f.Run(context.Background(), sessionID, prompt, attachments...)
}

func (f *fakeThreadCoordinator) CancelAll() {}

type fakeThreadHandle struct {
	id  string
	app *app.App
}

func (h *fakeThreadHandle) ID() string    { return h.id }
func (h *fakeThreadHandle) App() *app.App { return h.app }

// fakeThreadSpawner spawns a real (but network/db-free) app.App per call
// via app.NewForTest, wired with a fake Sessions/AgentCoordinator instead
// of the real ones a full bootstrap would build.
type fakeThreadSpawner struct {
	t *testing.T

	mu     sync.Mutex
	byPath map[string]*fakeThreadHandle
}

func newFakeThreadSpawner(t *testing.T) *fakeThreadSpawner {
	return &fakeThreadSpawner{t: t, byPath: make(map[string]*fakeThreadHandle)}
}

func (s *fakeThreadSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeThreadSessions{}
	a.AgentCoordinator = &fakeThreadCoordinator{}

	h := &fakeThreadHandle{id: path, app: a}
	s.byPath[path] = h
	return h, nil
}

func (s *fakeThreadSpawner) Release(ctx context.Context, id string) error {
	return nil
}

// newTestThreadAppWorkspace wires an AppWorkspace whose App has a real
// *thread.Manager attached over a real git repo, a real store, and the
// fakeThreadSpawner defined above.
func newTestThreadAppWorkspace(t *testing.T) (*AppWorkspace, *thread.Manager) {
	t.Helper()
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       newTestThreadStoreDB(t),
		Spawner:     newFakeThreadSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetThreadManager(mgr)

	store := config.NewTestStore(&config.Config{}, repo)
	return NewAppWorkspace(a, store), mgr
}

func TestAppWorkspace_SupportsThreads(t *testing.T) {
	aw, _ := newTestThreadAppWorkspace(t)
	require.True(t, aw.SupportsThreads())

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	plain := NewAppWorkspace(a, config.NewTestStore(&config.Config{}, t.TempDir()))
	require.False(t, plain.SupportsThreads())
}

func TestAppWorkspace_CreateListGetThread(t *testing.T) {
	aw, _ := newTestThreadAppWorkspace(t)
	ctx := t.Context()

	threads, err := aw.ListThreads(ctx)
	require.NoError(t, err)
	require.Empty(t, threads)

	created, err := aw.CreateThread(ctx, proto.CreateThreadRequest{
		Name: "test-thread",
		Goal: "do the thing",
	})
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, "test-thread", created.Name)
	require.Equal(t, "do the thing", created.Goal)

	got, err := aw.GetThread(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, created.Name, got.Name)
	require.Equal(t, created.Goal, got.Goal)

	threads, err = aw.ListThreads(ctx)
	require.NoError(t, err)
	require.Len(t, threads, 1)
	require.Equal(t, created.ID, threads[0].ID)
}

func TestAppWorkspace_AttachThread(t *testing.T) {
	aw, mgr := newTestThreadAppWorkspace(t)
	ctx := t.Context()

	created, err := aw.CreateThread(ctx, proto.CreateThreadRequest{
		Name: "attach-me",
		Goal: "do the thing",
	})
	require.NoError(t, err)

	handle := mgr.Handle(created.ID)
	require.NotNil(t, handle, "thread's workspace should be live right after Create")

	attached, detach, err := aw.AttachThread(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)

	attachedAW, ok := attached.(*AppWorkspace)
	require.True(t, ok)
	require.Same(t, handle.App(), attachedAW.App(), "AttachThread must bind to the thread's own nested App")
	require.NotSame(t, aw.App(), attachedAW.App())

	require.NotPanics(t, func() { detach() })
}

func TestAppWorkspace_AttachThread_UnknownID(t *testing.T) {
	aw, _ := newTestThreadAppWorkspace(t)

	_, _, err := aw.AttachThread(t.Context(), "no-such-thread")
	require.Error(t, err)
}

// TestAppWorkspace_TranslateEvent_ThreadLifecycle drives a real
// *thread.Manager through a create-then-remove lifecycle and verifies
// that AppWorkspace.translateEvent (the local-mode counterpart of
// ClientWorkspace.translateEvent's proto.ThreadEvent case) turns each
// raw pubsub.Event[thread.Event] the Manager publishes into the
// pubsub.Event[proto.Thread] that root.go/ui.go's Update() actually
// switches on. Before this fix, AppWorkspace.Subscribe forwarded the raw
// event untranslated, so a live thread status change never reached the
// TUI's docked panel/badge/completion toast — they only ever updated via
// TTL polling.
func TestAppWorkspace_TranslateEvent_ThreadLifecycle(t *testing.T) {
	aw, mgr := newTestThreadAppWorkspace(t)
	ctx := t.Context()

	events := mgr.Subscribe(ctx)

	created, err := aw.CreateThread(ctx, proto.CreateThreadRequest{
		Name: "lifecycle-thread",
		Goal: "do the thing",
	})
	require.NoError(t, err)

	require.NoError(t, aw.RemoveThread(ctx, created.ID, proto.RemoveThreadOptions{Force: true}))

	var sawCreated, sawRemoved bool
	for !sawCreated || !sawRemoved {
		select {
		case raw := <-events:
			translated := aw.translateEvent(raw)
			got, ok := translated.(pubsub.Event[proto.Thread])
			require.True(t, ok, "expected pubsub.Event[proto.Thread], got %T", translated)
			if got.Payload.ID != created.ID {
				continue
			}
			switch got.Type {
			case pubsub.CreatedEvent:
				sawCreated = true
			case pubsub.DeletedEvent:
				sawRemoved = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for translated created/removed thread events")
		}
	}
}
