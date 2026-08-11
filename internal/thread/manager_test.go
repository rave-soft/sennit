package thread

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/stretchr/testify/require"
)

// -- git test helpers (mirrors internal/git/git_test.go's style; thread
// tests need real repos too but git.go's own `run` helper is unexported to
// its package, so this shells out directly). --

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}

func initRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, dir, "README.md", "hello\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

// -- store/manager scaffolding --

func newTestStoreDB(t *testing.T) Store {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return NewStore(db.New(conn), dataDir)
}

// newTestManager wires a Manager over a real store, a real git repo (repo),
// and the fakeSpawner defined below.
func newTestManager(t *testing.T, repo string) (*Manager, *fakeSpawner) {
	t.Helper()
	spawner := newFakeSpawner(t)
	mgr := NewManager(ManagerOptions{
		Store:       newTestStoreDB(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	return mgr, spawner
}

// -- fakes --

// fakeSessions implements the session creation methods used by the manager.
type fakeSessions struct {
	session.Service
	mu             sync.Mutex
	n              int
	createdSession session.Session
}

func (f *fakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

func (f *fakeSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdSession = session.Session{ID: id, ParentSessionID: parentSessionID, Title: title}
	return f.createdSession, nil
}

// fakeCoordinator implements agent.Coordinator, recording Run calls and
// CancelAll invocations. It does not publish RunComplete itself — tests
// drive that explicitly through the owning app's RunCompletions broker, so
// each test controls exactly when a thread's run finishes.
type fakeCoordinator struct {
	agent.Coordinator

	mu              sync.Mutex
	runs            []fakeRun
	cancelAllCalled bool
	runErr          error
}

type fakeRun struct {
	sessionID string
	prompt    string
	runID     string
}

func (f *fakeCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, fakeRun{sessionID: sessionID, prompt: prompt, runID: agent.RunIDFromContext(ctx)})
	return nil, f.runErr
}

func (f *fakeCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *fakeCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, fakeRun{sessionID: sessionID, prompt: prompt, runID: agent.RunIDFromContext(ctx)})
	err := f.runErr
	f.mu.Unlock()
	return nil, err
}

func (f *fakeCoordinator) CancelAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelAllCalled = true
}

func (f *fakeCoordinator) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// fakeHandle is the Handle returned by fakeSpawner.
type fakeHandle struct {
	id  string
	app *app.App
}

func (h *fakeHandle) ID() string    { return h.id }
func (h *fakeHandle) App() *app.App { return h.app }

// fakeSpawner spawns a real (but network/db-free) app.App per call via
// app.NewForTest, wired with fakeSessions and a fakeCoordinator instead of
// the real ones a full bootstrap would build. It keeps every spawned app
// reachable by worktree path so tests can drive a thread's run to
// completion by publishing to that app's RunCompletions broker directly.
type fakeSpawner struct {
	t *testing.T

	mu           sync.Mutex
	byPath       map[string]*fakeHandle
	coordByPath  map[string]*fakeCoordinator
	released     map[string]bool
	releaseCount map[string]int
	spawnCount   int
	spawnErr     error
	runErr       error
	blockSpawn   bool
	spawnEntered chan struct{}
	spawnRelease chan struct{}
}

func newFakeSpawner(t *testing.T) *fakeSpawner {
	return &fakeSpawner{
		t:            t,
		byPath:       make(map[string]*fakeHandle),
		coordByPath:  make(map[string]*fakeCoordinator),
		released:     make(map[string]bool),
		releaseCount: make(map[string]int),
	}
}

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (Handle, error) {
	if s.blockSpawn {
		close(s.spawnEntered)
		<-ctx.Done()
		<-s.spawnRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spawnCount++
	if s.spawnErr != nil {
		return nil, s.spawnErr
	}

	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeSessions{}
	coord := &fakeCoordinator{runErr: s.runErr}
	a.AgentCoordinator = coord

	h := &fakeHandle{id: path, app: a}
	s.byPath[path] = h
	s.coordByPath[path] = coord
	return h, nil
}

func (s *fakeSpawner) spawns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnCount
}

func (s *fakeSpawner) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[id] = true
	s.releaseCount[id]++
	return nil
}

func (s *fakeSpawner) releases(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseCount[id]
}

func (s *fakeSpawner) appFor(path string) *app.App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPath[path].app
}

func (s *fakeSpawner) coordFor(path string) *fakeCoordinator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coordByPath[path]
}

func (s *fakeSpawner) wasReleased(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released[id]
}

// publishSuccess simulates a thread's agent run finishing successfully.
func publishSuccess(t *testing.T, a *app.App, sessionID string) {
	t.Helper()
	coord := a.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() > 0 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[len(coord.runs)-1].runID
	coord.mu.Unlock()
	a.RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: sessionID, RunID: runID, Text: "finished"})
}

// -- tests --

func TestNewManager_WorktreeDirResolution(t *testing.T) {
	repoRoot := filepath.Join(string(filepath.Separator), "home", "user", "myrepo")

	t.Run("empty defaults to sibling of repo root", func(t *testing.T) {
		mgr := NewManager(ManagerOptions{RepoRoot: repoRoot})
		require.Equal(t, filepath.Join(string(filepath.Separator), "home", "user", "myrepo-threads"), mgr.worktreeDir)
	})

	t.Run("relative resolves against repo root's parent", func(t *testing.T) {
		mgr := NewManager(ManagerOptions{RepoRoot: repoRoot, WorktreeDir: "../thread-worktrees"})
		require.Equal(t, filepath.Join(string(filepath.Separator), "home", "thread-worktrees"), mgr.worktreeDir)
	})

	t.Run("absolute is used as-is", func(t *testing.T) {
		abs := filepath.Join(string(filepath.Separator), "var", "tmp", "braid-threads")
		mgr := NewManager(ManagerOptions{RepoRoot: repoRoot, WorktreeDir: abs})
		require.Equal(t, abs, mgr.worktreeDir)
	})
}

func TestManager_CreateHappyPath(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	events := mgr.Subscribe(t.Context())

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "alpha",
		Goal:        "implement the thing",
		MergePolicy: MergeManual,
	})
	require.NoError(t, err)
	require.Equal(t, "alpha", st.Name)
	require.Equal(t, "main", st.BaseBranch)
	require.Equal(t, "thread/alpha", st.Branch)
	require.Equal(t, StatusRunning, st.Status)
	require.NotEmpty(t, st.SessionID)
	require.DirExists(t, st.WorktreePath)

	branch := runGit(t, repo, "branch", "--list", "thread/alpha")
	require.Contains(t, branch, "thread/alpha")

	coord := spawner.coordFor(st.WorktreePath)
	require.NotNil(t, coord)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)

	var gotCreated, gotRunning bool
	for i := 0; i < 2; i++ {
		select {
		case ev := <-events:
			switch ev.Payload.Type {
			case EventCreated:
				gotCreated = true
			case EventStatusChanged:
				if ev.Payload.Thread.Status == StatusRunning {
					gotRunning = true
				}
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for events")
		}
	}
	require.True(t, gotCreated)
	require.True(t, gotRunning)
}

func TestManager_CreateMarksAgentThreadSessionAsChild(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:            "child-session",
		Goal:            "implement the thing",
		MergePolicy:     MergeManual,
		ParentSessionID: "parent-session",
	})
	require.NoError(t, err)

	sessions := spawner.appFor(st.WorktreePath).Sessions.(*fakeSessions)
	sessions.mu.Lock()
	created := sessions.createdSession
	sessions.mu.Unlock()
	require.Equal(t, st.SessionID, created.ID)
	require.Equal(t, "parent-session", created.ParentSessionID)
}

func TestManager_CreateRejectsDuplicateName(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	_, err := mgr.Create(t.Context(), CreateArgs{Name: "dup", Goal: "x", MergePolicy: MergeManual})
	require.NoError(t, err)

	_, err = mgr.Create(t.Context(), CreateArgs{Name: "dup", Goal: "x", MergePolicy: MergeManual})
	require.Error(t, err)
}

func TestManager_CreateRejectsInvalidName(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	_, err := mgr.Create(t.Context(), CreateArgs{Name: "Not Valid!", Goal: "x"})
	require.Error(t, err)
}

func TestManager_ManualPolicyCompleted(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "beta", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "did the work\n")
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() >= 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID, Text: "finished"})

	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, st.Status)
	require.Equal(t, "finished", st.ResultSummary)
	require.NotZero(t, st.CompletedAt)
	require.NoFileExists(t, filepath.Join(repo, "output.txt"))

	require.NoError(t, mgr.Merge(t.Context(), st.ID))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)
	require.FileExists(t, filepath.Join(repo, "output.txt"))
}

func TestManager_RunCompleteSuccessAutoMerge(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	// Default MergePolicy is auto.
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "gamma", Goal: "do it"})
	require.NoError(t, err)
	require.Equal(t, MergeAuto, st.MergePolicy)

	writeFile(t, st.WorktreePath, "output.txt", "auto merged\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)
	require.FileExists(t, filepath.Join(repo, "output.txt"))

	threadTip := runGit(t, repo, "rev-parse", "thread/gamma")
	mainTip := runGit(t, repo, "rev-parse", "main")
	require.Equal(t, threadTip, mainTip)
}

func TestManager_ConflictAndRetryAfterResolution(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "delta", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	// The base branch (main, checked out in repo itself) and the thread
	// branch each edit README.md, guaranteeing a conflict on merge.
	writeFile(t, repo, "README.md", "main version\n")
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "edit on main")

	writeFile(t, st.WorktreePath, "README.md", "thread version\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	require.NoError(t, mgr.Merge(t.Context(), st.ID))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusConflict, st.Status)
	require.Contains(t, st.Error, "README.md")

	// Resolve: write the merged content and stage it, but don't commit —
	// Merge's own CommitAll step finishes the merge commit.
	writeFile(t, st.WorktreePath, "README.md", "resolved version\n")
	runGit(t, st.WorktreePath, "add", "README.md")

	require.NoError(t, mgr.Merge(t.Context(), st.ID))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)

	content, err := os.ReadFile(filepath.Join(repo, "README.md"))
	require.NoError(t, err)
	require.Equal(t, "resolved version\n", string(content))
}

func TestManager_MergeBlockedWhenBaseCheckedOutAndDirty(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	// main is checked out in repo (the primary worktree) throughout, so
	// the fast-forward step must fall back to the ff-only merge path —
	// which this test then blocks by leaving repo dirty.
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "epsilon", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	writeFile(t, repo, "dirty.txt", "uncommitted\n")

	require.NoError(t, mgr.Merge(t.Context(), st.ID))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMergeBlocked, st.Status)
	require.NotEmpty(t, st.Error)
}

func TestManager_FastForwardWhenBaseNotCheckedOut(t *testing.T) {
	repo := initRepo(t)
	// A base branch that is never checked out anywhere, so the
	// push-based FastForward succeeds directly without falling back to
	// the ff-only merge path.
	runGit(t, repo, "branch", "other-base")

	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{
		Name:        "zeta",
		Goal:        "do it",
		BaseBranch:  "other-base",
		MergePolicy: MergeManual,
	})
	require.NoError(t, err)
	require.Equal(t, "other-base", st.BaseBranch)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	require.NoError(t, mgr.Merge(t.Context(), st.ID))
	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, st.Status)

	threadTip := runGit(t, repo, "rev-parse", "thread/zeta")
	baseTip := runGit(t, repo, "rev-parse", "other-base")
	require.Equal(t, threadTip, baseTip)
}

func TestManager_WaitWakesOnCompletion(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "eta", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- mgr.Wait(context.Background(), []string{st.ID}, 5*time.Second)
	}()

	select {
	case <-done:
		t.Fatal("Wait returned before the thread reached a terminal state")
	case <-time.After(100 * time.Millisecond):
	}

	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not wake up after completion")
	}
}

func TestManager_Recover(t *testing.T) {
	repo := initRepo(t)
	store := newTestStoreDB(t)
	mgr := NewManager(ManagerOptions{
		Store:       store,
		Spawner:     newFakeSpawner(t),
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})

	running, err := store.Create(t.Context(), CreateParams{
		Name: "running-gone", Goal: "x", BaseBranch: "main",
		Branch: "thread/running-gone", WorktreePath: filepath.Join(t.TempDir(), "missing"),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), running.ID, SetStatusParams{Status: StatusRunning})
	require.NoError(t, err)

	presentWorktree := t.TempDir()
	stillRunning, err := store.Create(t.Context(), CreateParams{
		Name: "running-present", Goal: "x", BaseBranch: "main",
		Branch: "thread/running-present", WorktreePath: presentWorktree,
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), stillRunning.ID, SetStatusParams{Status: StatusRunning})
	require.NoError(t, err)

	merged, err := store.Create(t.Context(), CreateParams{
		Name: "already-merged", Goal: "x", BaseBranch: "main",
		Branch: "thread/already-merged", WorktreePath: filepath.Join(t.TempDir(), "also-missing"),
	})
	require.NoError(t, err)
	_, err = store.SetStatus(t.Context(), merged.ID, SetStatusParams{Status: StatusMerged, CompletedAt: 1})
	require.NoError(t, err)

	require.NoError(t, mgr.Recover(t.Context()))

	got, err := store.Get(t.Context(), running.ID)
	require.NoError(t, err)
	require.Equal(t, StatusFailed, got.Status)

	got, err = store.Get(t.Context(), stillRunning.ID)
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, got.Status)

	got, err = store.Get(t.Context(), merged.ID)
	require.NoError(t, err)
	require.Equal(t, StatusMerged, got.Status)
}

func TestManager_RemoveRefusesActiveWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "theta", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	err = mgr.Remove(t.Context(), st.ID, false, false)
	require.Error(t, err)
}

func TestManager_RemoveRefusesDirtyUnmergedWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "iota", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "uncommitted.txt", "x\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	err = mgr.Remove(t.Context(), st.ID, false, false)
	require.Error(t, err)
}

func TestManager_RemoveForce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "kappa", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)
	worktreePath := st.WorktreePath

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))

	require.True(t, spawner.wasReleased(worktreePath))
	require.NoDirExists(t, worktreePath)

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err)

	exists := runGit(t, repo, "branch", "--list", "thread/kappa")
	require.Empty(t, exists)
}

func TestManager_SendRedispatches(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "lambda", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	writeFile(t, st.WorktreePath, "output.txt", "content\n")
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, 2*time.Second))

	require.NoError(t, mgr.Send(t.Context(), st.ID, "keep going"))

	st, err = mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, st.Status)

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, 5*time.Millisecond)
}

func TestManager_HandleAndWorkspaceID(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)

	// Unknown thread: both accessors report "not spawned".
	require.Nil(t, mgr.Handle("no-such-id"))
	require.Empty(t, mgr.WorkspaceID("no-such-id"))

	st, err := mgr.Create(t.Context(), CreateArgs{Name: "mu", Goal: "do it", MergePolicy: MergeManual})
	require.NoError(t, err)

	// fakeHandle.ID() is the worktree path (see fakeSpawner.Spawn).
	h := mgr.Handle(st.ID)
	require.NotNil(t, h)
	require.Equal(t, st.WorktreePath, h.ID())
	require.Equal(t, st.WorktreePath, mgr.WorkspaceID(st.ID))

	require.NoError(t, mgr.Remove(t.Context(), st.ID, true, true))
	require.Nil(t, mgr.Handle(st.ID))
	require.Empty(t, mgr.WorkspaceID(st.ID))
}

// A successful Send into an idle thread must NOT release the workspace it
// just respawned (regression: the ownership-transfer defer used to fire on
// the success path), and a follow-up queued into a live run must survive
// the in-flight run's completion: ownership moves to the follow-up's
// RunID, so only its own completion releases the workspace and settles
// the thread's status.
func TestManager_SendOwnershipFollowsLatestRun(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "owner", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))
	require.Nil(t, mgr.Handle(st.ID))

	// Respawn via Send: the workspace must stay alive after Send returns.
	require.NoError(t, mgr.Send(t.Context(), st.ID, "first"))
	require.NotNil(t, mgr.Handle(st.ID))
	require.Zero(t, spawner.releases(st.WorktreePath)-1) // only the goal run's release so far

	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	ownerRunID := coord.runs[0].runID
	coord.mu.Unlock()

	// Queue a follow-up while the first run is in flight.
	require.NoError(t, mgr.Send(t.Context(), st.ID, "second"))
	require.Eventually(t, func() bool { return coord.runCount() == 2 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	followUpRunID := coord.runs[1].runID
	coord.mu.Unlock()
	require.NotEmpty(t, followUpRunID)
	require.NotEqual(t, ownerRunID, followUpRunID)

	// The first run completing must neither release the workspace nor
	// settle the thread: the queued follow-up still owns both.
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: ownerRunID, Text: "first done"})
	require.Never(t, func() bool { return mgr.Handle(st.ID) == nil }, 100*time.Millisecond, 10*time.Millisecond)
	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, StatusRunning, got.Status)

	// The follow-up completing settles everything.
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: followUpRunID, Text: "second done"})
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == StatusCompleted
	}, time.Second, time.Millisecond)
}

func TestManager_ConcurrentSendRespawnsOnce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "once", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { require.NoError(t, mgr.Send(t.Context(), st.ID, "again")) })
	}
	wg.Wait()
	require.Equal(t, 2, spawner.spawns())
	coord := spawner.coordFor(st.WorktreePath)
	// coordFor returns the coordinator of the respawned workspace (each
	// Spawn builds a fresh one): one respawned run from the first Send
	// plus eleven queued follow-ups — each dispatched asynchronously, so
	// wait for all of them to land before completing them.
	require.Eventually(t, func() bool { return coord.runCount() == 12 }, time.Second, time.Millisecond)
	// Every dispatched turn eventually publishes its own RunComplete. The
	// workspace is released only by the turn that currently owns the
	// runtime (the last dispatched RunID) — completing all of them models
	// that and asserts exactly one effective release.
	coord.mu.Lock()
	runIDs := make([]string, 0, len(coord.runs))
	for _, r := range coord.runs {
		runIDs = append(runIDs, r.runID)
	}
	coord.mu.Unlock()
	for _, id := range runIDs {
		spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: id, Text: "finished"})
	}
	require.Eventually(t, func() bool { return mgr.Handle(st.ID) == nil }, time.Second, time.Millisecond)
	require.GreaterOrEqual(t, spawner.releases(st.WorktreePath), 2)
}

func TestManager_CancelledRunCompleteWinsOverError(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "cancel-error", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID, Error: "cancelled", Cancelled: true})
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == StatusInterrupted
	}, time.Second, time.Millisecond)
}

func TestManager_RunAcceptedImmediateErrorCompletesAndReleases(t *testing.T) {
	repo := initRepo(t)
	spawner := newFakeSpawner(t)
	mgr := NewManager(ManagerOptions{
		Store:       newTestStoreDB(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	// Configure the coordinator created by Spawn before Create dispatches.
	// Its RunAccepted returns this error and deliberately publishes no event.
	spawner.runErr = errors.New("boom")
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "immediate-error", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := mgr.Get(t.Context(), st.ID)
		return err == nil && got.Status == StatusFailed && strings.Contains(got.Error, "boom")
	}, time.Second, time.Millisecond)
	require.Nil(t, mgr.Handle(st.ID))
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
	require.NoError(t, mgr.Shutdown(t.Context()))
}

func TestManager_ConcurrentShutdownClosesMutations(t *testing.T) {
	repo := initRepo(t)
	mgr, _ := newTestManager(t, repo)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { require.NoError(t, mgr.Shutdown(t.Context())) })
	}
	wg.Wait()
	_, err := mgr.Create(t.Context(), CreateArgs{Name: "closed", Goal: "go"})
	require.ErrorIs(t, err, ErrManagerClosed)
	require.ErrorIs(t, mgr.Send(t.Context(), "missing", "go"), ErrManagerClosed)
	require.ErrorIs(t, mgr.Merge(t.Context(), "missing"), ErrManagerClosed)
	require.ErrorIs(t, mgr.Remove(t.Context(), "missing", true, false), ErrManagerClosed)
	require.ErrorIs(t, mgr.Recover(t.Context()), ErrManagerClosed)
}

func TestManager_ShutdownWaitsForCancelledSpawnRollback(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "blocked", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	publishSuccess(t, spawner.appFor(st.WorktreePath), st.SessionID)
	require.NoError(t, mgr.Wait(t.Context(), []string{st.ID}, time.Second))

	spawner.blockSpawn = true
	spawner.spawnEntered = make(chan struct{})
	spawner.spawnRelease = make(chan struct{})
	sendDone := make(chan error, 1)
	go func() { sendDone <- mgr.Send(context.Background(), st.ID, "again") }()
	<-spawner.spawnEntered
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- mgr.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before Spawn resolved: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(spawner.spawnRelease)
	require.Error(t, <-sendDone)
	require.NoError(t, <-shutdownDone)
	require.Nil(t, mgr.Handle(st.ID))
	require.Equal(t, 2, spawner.releases(st.WorktreePath), "the completed original and cancelled respawn are each released once")
}

func TestManager_RemoveAndCompletionReleaseOnce(t *testing.T) {
	repo := initRepo(t)
	mgr, spawner := newTestManager(t, repo)
	st, err := mgr.Create(t.Context(), CreateArgs{Name: "race-remove", Goal: "go", MergePolicy: MergeManual})
	require.NoError(t, err)
	coord := spawner.coordFor(st.WorktreePath)
	require.Eventually(t, func() bool { return coord.runCount() == 1 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[0].runID
	coord.mu.Unlock()
	var wg sync.WaitGroup
	wg.Go(func() { _ = mgr.Remove(t.Context(), st.ID, true, true) })
	wg.Go(func() {
		spawner.appFor(st.WorktreePath).RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: st.SessionID, RunID: runID})
	})
	wg.Wait()
	require.Equal(t, 1, spawner.releases(st.WorktreePath))
}
