package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/thread"
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

	// Not t.TempDir(): its cleanup fails the test intermittently with
	// "unlinkat .git: directory not empty" once a real app.Bootstrap has
	// run against this repo. The .git directory is empty by the time the
	// failure is inspected, which is the signature of something creating
	// and removing files inside it while the removal walks — a
	// background subsystem the bootstrap starts that outlives
	// App.Shutdown's join. I did not identify which one, so this is a
	// mitigation, not a root-cause fix: retry the removal briefly and
	// give up quietly rather than failing a test for a reason unrelated
	// to what it asserts. Removing the /tmp entry is best-effort anyway.
	dir, err := os.MkdirTemp("", "sennit-threads-test-")
	require.NoError(t, err)
	t.Cleanup(func() {
		for range 20 {
			if err := os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
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
	return threadspawn.NewStore(db.New(conn), dataDir)
}

// -- fakes: mirror internal/thread/manager_test.go's fakeSessions,
// fakeCoordinator, fakeHandle, fakeSpawner (unexported to package thread,
// so reimplemented here against the exported thread.Spawner/thread.Handle
// interfaces). --

type fakeThreadSessions struct {
	session.Service
	mu    sync.Mutex
	n     int
	sesss map[string]session.Session
}

func newFakeThreadSessions() *fakeThreadSessions {
	return &fakeThreadSessions{sesss: make(map[string]session.Session)}
}

func (f *fakeThreadSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	s := session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}
	f.sesss[s.ID] = s
	return s, nil
}

func (f *fakeThreadSessions) Get(_ context.Context, sessionID string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sesss[sessionID]
	if !ok {
		return session.Session{}, fmt.Errorf("session %q not found", sessionID)
	}
	return s, nil
}

func (f *fakeThreadSessions) List(_ context.Context) ([]session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sesss := make([]session.Session, 0, len(f.sesss))
	for _, s := range f.sesss {
		sesss = append(sesss, s)
	}
	return sesss, nil
}

func (f *fakeThreadSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := session.Session{ID: id, ParentSessionID: parentSessionID, Title: title}
	f.sesss[s.ID] = s
	return s, nil
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

func (f *fakeThreadCoordinator) Cancel(sessionID string) {}

func (f *fakeThreadCoordinator) IsBusy() bool { return false }

func (f *fakeThreadCoordinator) SetDelegationTools(tools.ThreadManager, tools.TaskManager) {}

func (f *fakeThreadCoordinator) RegisterDelegationParent(string, agent.DelegationParent) {}

func (f *fakeThreadCoordinator) SendToParent(context.Context, string, string) error { return nil }

type fakeThreadHandle struct {
	id  string
	app *app.App
}

func (h *fakeThreadHandle) ID() string    { return h.id }
func (h *fakeThreadHandle) App() *app.App { return h.app }
func (h *fakeThreadHandle) Workspace() thread.Workspace {
	return &threadspawn.AppWorkspaceAdapter{App: h.app}
}

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
	a.SetSessionsForTest(newFakeThreadSessions())
	a.AgentCoordinator = &fakeThreadCoordinator{}

	h := &fakeThreadHandle{id: path, app: a}
	s.byPath[path] = h
	return h, nil
}

func (s *fakeThreadSpawner) Release(ctx context.Context, id string) error {
	return nil
}

// shutdownManagerOnCleanup registers a t.Cleanup that shuts mgr down on a
// bounded context and fails the test if Shutdown does not return cleanly.
// A Manager owns background goroutines (auto-merge, delivery, worktree
// removal) that keep touching a thread's worktree - and the
// repo's own .git directory - after the test body returns; App.Shutdown/
// ShutdownForTest does NOT join these, since publishing the manager
// through SetDelegationManagers registers no App-owned cleanup for it.
// Without this, a
// t.TempDir() RemoveAll can race one of those goroutines and fail with
// "directory not empty". Call this AFTER every t.TempDir() call the
// manager's worktrees live under: t.Cleanup runs LIFO, so it must be the
// most recently registered cleanup to run first.
func shutdownManagerOnCleanup(t *testing.T, mgr *thread.Manager) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, mgr.Shutdown(ctx))
	})
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
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	store := configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo))
	return NewAppWorkspace(a, store), mgr
}

func TestAppWorkspace_SupportsThreads(t *testing.T) {
	aw, _ := newTestThreadAppWorkspace(t)
	require.True(t, aw.SupportsThreads())

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	plain := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))
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

	// The attached workspace wraps the thread's own AppWorkspace so the
	// person's turns route through the Manager (see
	// attachedThreadWorkspace); unwrap it to reach the App underneath.
	wrapped, ok := attached.(*attachedThreadWorkspace)
	require.True(t, ok)
	attachedAW, ok := wrapped.Workspace.(*AppWorkspace)
	require.True(t, ok)
	fh, ok := handle.(*fakeThreadHandle)
	require.True(t, ok)
	require.Same(t, fh.app, attachedAW.App(), "AttachThread must bind to the thread's own nested App")
	require.NotSame(t, aw.App(), attachedAW.App())

	require.NotPanics(t, func() { detach() })
}

func TestAppWorkspace_AttachThread_UnknownID(t *testing.T) {
	aw, _ := newTestThreadAppWorkspace(t)

	_, _, err := aw.AttachThread(t.Context(), "no-such-thread")
	require.Error(t, err)
}

// TestAppWorkspace_AttachThread_CompletedThread verifies that attaching
// to a thread whose run has finished reactivates it: the workspace is
// respawned and the caller gets a writable workspace, so the user can
// keep working in the thread by hand instead of only reading it.
func TestAppWorkspace_AttachThread_CompletedThread(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	fs := newFakeThreadSessions()
	a.SetSessionsForTest(fs)

	store := newTestThreadStoreDB(t)
	spawner := newFakeThreadSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	// Pre-populate the fake session store so the attached workspace
	// can read the session through the main app.
	_, err := fs.Create(t.Context(), "complete-me")
	require.NoError(t, err)

	// Insert a completed thread directly into the store (bypassing
	// Create so no workspace was ever spawned — Handle returns nil).
	created, err := store.Create(t.Context(), thread.CreateParams{
		Name:         "complete-me",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/complete-me",
		WorktreePath: t.TempDir(),
		SessionID:    "sess-1",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), created.ID, thread.SetStatusParams{
		Status:        thread.StatusCompleted,
		ResultSummary: "did the thing",
	})
	require.NoError(t, err)

	// Handle returns nil (no runtime was ever installed).
	require.Nil(t, mgr.Handle(created.ID))

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo)))
	attached, detach, err := aw.AttachThread(t.Context(), created.ID)
	require.NoError(t, err, "AttachThread for completed thread should return a workspace")
	require.NotNil(t, attached)
	require.NotNil(t, detach)
	require.NotPanics(t, detach)

	// The thread was reactivated: a writable workspace bound to the
	// thread's own respawned app, not the read-only view.
	// A writable workspace over the thread's own respawned app, not the
	// read-only view — wrapped so the turns the person starts in it are
	// dispatched through the Manager (see attachedThreadWorkspace).
	require.IsType(t, &attachedThreadWorkspace{}, attached, "attaching to a finished thread should reactivate it")
	require.NotNil(t, mgr.Handle(created.ID), "reactivation should install a runtime")

	// It now rests at idle, and the finished run's summary survives.
	got, err := aw.GetThread(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, string(thread.StatusIdle), got.Status)
	require.Equal(t, "did the thing", got.ResultSummary, "reactivation must not erase the earlier run's result")
}

// TestAppWorkspace_AttachThread_LiveThread verifies that AttachThread for
// a live thread succeeds and returns a Workspace backed by the thread's
// own App.
func TestAppWorkspace_AttachThread_LiveThread(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	spawner := newFakeThreadSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       newTestThreadStoreDB(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	// Create via manager to get a real handle.
	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:       "live-thread",
		Goal:       "do the thing",
		BaseBranch: "main",
	})
	require.NoError(t, err)

	// Handle must be non-nil (workspace is spawned).
	handle := mgr.Handle(st.ID)
	require.NotNil(t, handle)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo)))
	attached, detach, err := aw.AttachThread(t.Context(), st.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)

	// Detach should be a no-op (no workspace to release in local mode).
	require.NotPanics(t, detach)
}

// TestAppWorkspace_TranslateEvent_ThreadLifecycle drives a real
// *thread.Manager through a create-then-remove lifecycle and verifies
// that AppWorkspace.translateEvent turns each
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

// TestAppWorkspace_AttachThread_MergedThread_ReadMessages verifies the
// read-only fallback: a thread in the merge flow is deliberately refused
// reactivation, so attaching to it still yields a read-only workspace
// whose session metadata is read from the shared database via the main
// app's session store.
func TestAppWorkspace_AttachThread_MergedThread_ReadMessages(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	fs := newFakeThreadSessions()
	a.SetSessionsForTest(fs)

	store := newTestThreadStoreDB(t)
	spawner := newFakeThreadSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	// Pre-populate the fake session store.
	_, err := fs.Create(t.Context(), "read-msgs")
	require.NoError(t, err)

	// Create a completed thread directly in the store.
	created, err := store.Create(t.Context(), thread.CreateParams{
		Name:         "read-msgs",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/read-msgs",
		WorktreePath: t.TempDir(),
		SessionID:    "sess-1",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), created.ID, thread.SetStatusParams{
		Status: thread.StatusMerged,
	})
	require.NoError(t, err)

	require.Nil(t, mgr.Handle(created.ID), "handle should be nil after completion")

	// Attach to the merged thread — reactivation is refused, so this
	// falls back to a read-only workspace.
	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo)))
	attached, detach, err := aw.AttachThread(t.Context(), created.ID)
	require.NoError(t, err)
	require.NotNil(t, attached)
	require.NotNil(t, detach)
	require.NotPanics(t, detach)
	require.IsType(t, &readOnlyWorkspace{}, attached, "merge-flow threads must not be reactivated")
	require.Nil(t, mgr.Handle(created.ID), "refused reactivation must not spawn a workspace")

	// The attached workspace can read the persisted session from the
	// shared session store via the main app.
	sess, err := attached.GetSession(t.Context(), "sess-1")
	require.NoError(t, err, "GetSession on merged thread should succeed")
	require.Equal(t, "read-msgs", sess.Title)
}

// TestAppWorkspace_AttachThread_MergedThread_IsReadOnly verifies that the
// workspace returned for a thread that cannot be reactivated (one in the
// merge flow) is read-only: all mutating operations return
// ErrReadOnlyOperation, and shutdown of the attached workspace does not
// affect the parent.
func TestAppWorkspace_AttachThread_MergedThread_IsReadOnly(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	fs := newFakeThreadSessions()
	a.SetSessionsForTest(fs)

	store := newTestThreadStoreDB(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeThreadSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	_, err := fs.Create(t.Context(), "readonly-check")
	require.NoError(t, err)

	created, err := store.Create(t.Context(), thread.CreateParams{
		Name:         "readonly-check",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/readonly-check",
		WorktreePath: t.TempDir(),
		SessionID:    "sess-1",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), created.ID, thread.SetStatusParams{
		Status: thread.StatusMerged,
	})
	require.NoError(t, err)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo)))
	attached, _, err := aw.AttachThread(t.Context(), created.ID)
	require.NoError(t, err)

	// Verify it's a read-only workspace.
	require.True(t, IsReadOnlyError(attached.AgentRun(t.Context(), "sess-1", "hello")))
	_, err = attached.CreateThread(t.Context(), proto.CreateThreadRequest{})
	require.True(t, IsReadOnlyError(err))
	require.True(t, IsReadOnlyError(attached.UpdatePreferredModel(config.ScopeWorkspace, config.SelectedModel{})))

	// Verify WorkingDir is the thread worktree, not the parent's.
	require.Equal(t, created.WorktreePath, attached.WorkingDir())

	// Shutdown the read-only workspace — should NOT affect parent.
	attached.Shutdown()
	_, err = aw.ListThreads(t.Context())
	require.NoError(t, err, "parent workspace should still be functional after attached shutdown")
}

// TestAppWorkspace_AttachThread_ReadOnlyRefusalNamesWhyItIsReadOnly pins
// the half of the fallback the user actually meets. Opening a thread that
// cannot be reactivated succeeds and looks ordinary; the refusal only
// arrives later, when they type into it. Naming just the operation there
// ("AgentRun is not allowed") describes a decision taken silently minutes
// earlier and leaves the reason reachable only by turning on debug logging.
func TestAppWorkspace_AttachThread_ReadOnlyRefusalNamesWhyItIsReadOnly(t *testing.T) {
	repo := initRepoForWorkspaceThreadsTest(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	fs := newFakeThreadSessions()
	a.SetSessionsForTest(fs)

	store := newTestThreadStoreDB(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     newFakeThreadSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetDelegationManagers(mgr, nil)
	shutdownManagerOnCleanup(t, mgr)

	_, err := fs.Create(t.Context(), "reason-check")
	require.NoError(t, err)
	created, err := store.Create(t.Context(), thread.CreateParams{
		Name:         "reason-check",
		Goal:         "do the thing",
		BaseBranch:   "main",
		Branch:       "thread/reason-check",
		WorktreePath: t.TempDir(),
		SessionID:    "sess-1",
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), created.ID, thread.SetStatusParams{Status: thread.StatusMerged})
	require.NoError(t, err)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(repo)))
	attached, _, err := aw.AttachThread(t.Context(), created.ID)
	require.NoError(t, err)

	runErr := attached.AgentRun(t.Context(), "sess-1", "hello")
	require.True(t, IsReadOnlyError(runErr))
	require.Contains(t, runErr.Error(), "AgentRun", "the refusal must still name the operation")
	require.Contains(t, runErr.Error(), "merge flow", "the refusal must carry why reactivation was refused")
}

// TestAppWorkspace_PermissionAnswerRoutesToTheThreadHoldingIt covers the
// return half of the thread-permission relay. A thread's prompt is raised
// inside its own isolated workspace and only displayed here, so answering
// against this workspace's own permission service would find no such
// request and quietly do nothing — leaving the thread blocked on the very
// prompt the user just answered.
func TestAppWorkspace_PermissionAnswerRoutesToTheThreadHoldingIt(t *testing.T) {
	ws, mgr := newTestThreadAppWorkspace(t)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "waiting",
		Goal:        "do the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	threadApp := mgr.PermissionsFor(st.ID)
	require.NotNil(t, threadApp, "the thread's own permission service must be live")

	// Watch the thread's own service so the test answers the real
	// request, id and all — the same value the relay hands the parent.
	raised := threadApp.Subscribe(t.Context())

	ctx := permission.WithDelegation(t.Context(), permission.DelegationRef{
		ID: st.ID, Name: st.Name, Kind: string(st.Kind),
	})
	granted := make(chan bool, 1)
	go func() {
		ok, _ := threadApp.Request(ctx, permission.CreatePermissionRequest{
			SessionID:  st.SessionID,
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       st.WorktreePath,
		})
		granted <- ok
	}()

	var req permission.PermissionRequest
	select {
	case ev := <-raised:
		req = ev.Payload
	case <-time.After(5 * time.Second):
		t.Fatal("the thread never raised its permission request")
	}
	require.Equal(t, st.ID, req.Delegation.ID, "precondition: the request is attributed to its thread")

	require.True(t, ws.PermissionGrant(req),
		"granting through the parent workspace must reach the thread's own service")

	select {
	case ok := <-granted:
		require.True(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("the thread stayed blocked after the parent granted its request")
	}
}

// The mirror image of the test above, and the one that was missing. While
// the user is drilled into a thread, the router hands every event to that
// thread's screen -- including a prompt the parent workspace raised behind
// it, whose agent goes on working. Answering it reached only the thread's
// own service, which is not holding that request, so the prompt could
// never be answered or dismissed: every click reported "permission
// response was not accepted".
func TestAttachedThread_PermissionAnswerReachesTheParentThatRaisedIt(t *testing.T) {
	ws, mgr := newTestThreadAppWorkspace(t)

	st, err := mgr.Create(t.Context(), thread.CreateArgs{
		Name:        "attached",
		Goal:        "do the thing",
		MergePolicy: thread.MergeManual,
	})
	require.NoError(t, err)

	attached, detach, err := ws.AttachThread(t.Context(), st.ID)
	require.NoError(t, err)
	t.Cleanup(detach)

	// A prompt from the parent's own turn: no delegation tag, and it is
	// the parent's service that blocks on it.
	parentPerms := ws.app.Permissions()
	raised := parentPerms.Subscribe(t.Context())
	granted := make(chan bool, 1)
	go func() {
		ok, _ := parentPerms.Request(t.Context(), permission.CreatePermissionRequest{
			SessionID:  "parent-session",
			ToolCallID: "call-1",
			ToolName:   "bash",
			Action:     "execute",
			Path:       ws.WorkingDir(),
		})
		granted <- ok
	}()

	var req permission.PermissionRequest
	select {
	case ev := <-raised:
		req = ev.Payload
	case <-time.After(5 * time.Second):
		t.Fatal("the parent never raised its permission request")
	}
	require.Empty(t, req.Delegation.ID, "precondition: this is the parent's own turn")

	require.True(t, attached.PermissionGrant(req),
		"answering on the thread's screen must still reach the service that raised the prompt")

	select {
	case ok := <-granted:
		require.True(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("the parent stayed blocked after its request was answered from the thread's screen")
	}
}
