package tools_test

// Exercises the thread_* tools against a real thread.Manager, using the
// same minimal git-repo + fake-spawner scaffolding internal/thread's own
// manager_test.go builds (those helpers are unexported to that package, so
// they're replicated here rather than imported). This package must import
// internal/thread as an external test package (package tools_test, not
// tools): internal/thread imports internal/app, which imports
// internal/agent, which imports internal/agent/tools — an internal test
// file (package tools) doing the same import would close that cycle.

import (
	"context"
	"encoding/json"
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
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func initRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

// fakeSessions implements just enough of sessionstore.Service for Manager.Create
// to mint a thread session.
type fakeSessions struct {
	sessionstore.Service
	mu sync.Mutex
	n  int
}

func (f *fakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

func (f *fakeSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	return session.Session{ID: id, ParentSessionID: parentSessionID, Title: title}, nil
}

// fakeCoordinator implements agent.Coordinator with no-op behavior beyond
// Run — thread's own dispatch/RunComplete wiring is exercised by
// internal/thread's tests, not here.
//
// Dispatch cannot actually succeed against it, and that is worth knowing
// before writing a test here: a thread's run is admitted through an
// acceptance reservation, BeginAccepted below has no way to mint one the
// threadspawn adapter will take (its fields are unexported, and the
// adapter checks it came from the coordinator it wraps), so every
// dispatched goal fails at once with ErrInvalidAcceptedRun and the
// thread lands in "failed". A test that needs a thread in some live
// status must put it there itself (SetStatusForTest) rather than create
// one with a goal and hope to look while it is still on its way there.
type fakeCoordinator struct {
	agent.Coordinator
}

func (f *fakeCoordinator) Run(_ context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *fakeCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return f.Run(ctx, sessionID, prompt, attachments...)
}

func (f *fakeCoordinator) CancelAll()                                                {}
func (f *fakeCoordinator) Cancel(sessionID string)                                   {}
func (f *fakeCoordinator) SetDelegationTools(tools.ThreadManager, tools.TaskManager) {}

// The send path reads these to report whether a message lands as the next
// turn or waits behind one in flight (see tools.SendOutcome). This fake
// never runs anything, so its sessions are never busy.
func (f *fakeCoordinator) IsSessionBusy(string) bool { return false }
func (f *fakeCoordinator) QueuedPrompts(string) int  { return 0 }

// fakeHandle/fakeSpawner spawn a real (network/db-free) app.App per thread
// via app.NewForTest, matching internal/thread/manager_test.go's approach.
type fakeHandle struct {
	id  string
	app *app.App
}

func (h *fakeHandle) ID() string    { return h.id }
func (h *fakeHandle) App() *app.App { return h.app }
func (h *fakeHandle) Workspace() thread.Workspace {
	return &threadspawn.AppWorkspaceAdapter{App: h.app}
}

type fakeSpawner struct {
	t *testing.T
}

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(&fakeSessions{})
	a.SetAgentCoordinatorForTest(&fakeCoordinator{})
	return &fakeHandle{id: path, app: a}, nil
}

func (s *fakeSpawner) Release(ctx context.Context, id string) error { return nil }

// fakeStore is an in-memory thread.Store, avoiding a real DB connection
// for these tool-level tests.
type fakeStore struct {
	mu    sync.Mutex
	byID  map[string]thread.Thread
	names map[string]string
	seq   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: make(map[string]thread.Thread), names: make(map[string]string)}
}

func (s *fakeStore) Create(_ context.Context, p thread.CreateParams) (thread.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	kind := p.Kind
	if kind == "" {
		kind = thread.KindThread
	}
	mergePolicy := p.MergePolicy
	if mergePolicy == "" && kind == thread.KindThread {
		mergePolicy = thread.MergeAuto
	}
	st := thread.Thread{
		Delegation: thread.Delegation{
			ID:        fmt.Sprintf("id-%d", s.seq),
			Name:      p.Name,
			Goal:      p.Goal,
			SessionID: p.SessionID,
			Status:    thread.StatusPending,
			Kind:      kind,
		},
		BaseBranch:   p.BaseBranch,
		Branch:       p.Branch,
		WorktreePath: p.WorktreePath,
		MergePolicy:  mergePolicy,
	}
	s.byID[st.ID] = st
	s.names[st.Name] = st.ID
	return st, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (thread.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return thread.Thread{}, fmt.Errorf("thread: not found: %s", id)
	}
	return st, nil
}

func (s *fakeStore) GetByName(ctx context.Context, name string) (thread.Thread, error) {
	s.mu.Lock()
	id, ok := s.names[name]
	s.mu.Unlock()
	if !ok {
		return thread.Thread{}, fmt.Errorf("thread: not found: %s", name)
	}
	return s.Get(ctx, id)
}

func (s *fakeStore) List(_ context.Context) ([]thread.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]thread.Thread, 0, len(s.byID))
	for _, st := range s.byID {
		out = append(out, st)
	}
	return out, nil
}

// ListAll is identical to List here: this fake never writes anything but
// KindThread rows, so there is no other kind to distinguish.
func (s *fakeStore) ListAll(ctx context.Context) ([]thread.Thread, error) {
	return s.List(ctx)
}

func (s *fakeStore) SetStatus(_ context.Context, id string, p thread.SetStatusParams) (thread.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return thread.Thread{}, fmt.Errorf("thread: not found: %s", id)
	}
	st.Status = p.Status
	st.Error = p.Error
	st.ResultSummary = p.ResultSummary
	st.CompletedAt = p.CompletedAt
	s.byID[id] = st
	return st, nil
}

func (s *fakeStore) SetSession(_ context.Context, id, sessionID string) (thread.Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return thread.Thread{}, fmt.Errorf("thread: not found: %s", id)
	}
	st.SessionID = sessionID
	s.byID[id] = st
	return st, nil
}

func (s *fakeStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return nil
	}
	delete(s.names, st.Name)
	delete(s.byID, id)
	return nil
}

// newTestThreadManager wires a Manager over a real git repo, an in-memory
// store, and the fake spawner above, and returns it already adapted to
// tools.ThreadManager for direct use by the thread_* tool constructors.
func newTestThreadManager(t *testing.T, repo string) tools.ThreadManager {
	t.Helper()
	mgr, _ := newTestThreadManagerWithStore(t, repo)
	return mgr
}

// newTestThreadManagerWithStore is newTestThreadManager plus the store
// behind it, for the one test that needs to seed a row the manager's own
// API cannot create - a task, which shares the table with threads.
func newTestThreadManagerWithStore(t *testing.T, repo string) (tools.ThreadManager, *fakeStore) {
	mgr, store, _ := newTestThreadManagerRaw(t, repo)
	return mgr, store
}

// newTestThreadManagerRaw also returns the thread.Manager behind the
// adapter, for the test that has to put a thread into a status these
// fakes never reach on their own.
func newTestThreadManagerRaw(t *testing.T, repo string) (tools.ThreadManager, *fakeStore, *thread.Manager) {
	t.Helper()
	store := newFakeStore()
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       store,
		Spawner:     &fakeSpawner{t: t},
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	// Manager owns background goroutines (auto-merge, delivery, worktree
	// removal) that keep touching repo/WorktreeDir after the test
	// body returns; join them before t.TempDir() removes those
	// directories, or RemoveAll can race a live writer. Registered after
	// the WorktreeDir/repo TempDirs so it runs first (t.Cleanup is LIFO).
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, mgr.Shutdown(ctx))
	})
	return threadspawn.AsAgentToolManager(mgr), store, mgr
}

// grantingPermissions always grants, skipping the interactive prompt path
// entirely — sufficient for tool tests that only need to verify the
// underlying manager call happened.
func grantingPermissions(t *testing.T) permission.Service {
	t.Helper()
	return permission.NewPermissionService(t.TempDir(), true, nil)
}

func callTool(t *testing.T, tool fantasy.AgentTool, params any) fantasy.ToolResponse {
	t.Helper()
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "session-1")

	input, err := json.Marshal(params)
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	return resp
}

// noTasks is an empty tools.TaskManager for the delegation tools, which
// answer for both kinds: these tests are about the thread half, and a
// workspace can genuinely have threads and no background tasks.
type noTasks struct{}

func (noTasks) Create(context.Context, tools.TaskCreateArgs) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, fmt.Errorf("thread_tools_test: no task manager")
}
func (noTasks) List(context.Context) ([]tools.TaskInfo, error) { return nil, nil }
func (noTasks) Get(context.Context, string) (tools.TaskInfo, error) {
	return tools.TaskInfo{}, fmt.Errorf("thread_tools_test: no task manager")
}
func (noTasks) Cancel(context.Context, string, string) error { return nil }
func (noTasks) Send(context.Context, string, string) (tools.SendOutcome, error) {
	return tools.SendOutcome{}, fmt.Errorf("thread_tools_test: no task manager")
}

func (noTasks) Output(context.Context, string, int) (tools.TaskOutput, error) {
	return tools.TaskOutput{}, fmt.Errorf("thread_tools_test: no task manager")
}

func TestThreadCreateTool_CreatesThread(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)
	tool := tools.NewThreadCreateTool(mgr, grantingPermissions(t))

	resp := callTool(t, tool, tools.ThreadCreateParams{Name: "alpha", Goal: "do the thing"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "alpha")

	threads, err := mgr.List(t.Context())
	require.NoError(t, err)
	require.Len(t, threads, 1)
	require.Equal(t, "alpha", threads[0].Name)
}

func TestThreadCreateTool_MissingFields(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)
	tool := tools.NewThreadCreateTool(mgr, grantingPermissions(t))

	resp := callTool(t, tool, tools.ThreadCreateParams{Goal: "do the thing"})
	require.True(t, resp.IsError)

	resp = callTool(t, tool, tools.ThreadCreateParams{Name: "alpha"})
	require.True(t, resp.IsError)
}

func TestThreadListTool_ListsCreatedThreads(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	_, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "beta", Goal: "x"})
	require.NoError(t, err)

	tool := tools.NewAgentListTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "beta")
}

func TestThreadListTool_EmptyWhenNoThreads(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewAgentListTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No delegations")
}

func TestThreadStatusTool_ReturnsDetails(t *testing.T) {
	repo := initRepo(t)
	mgr, _, raw := newTestThreadManagerRaw(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "gamma"})
	require.NoError(t, err)
	// A status the thread is actually in, and stays in - see
	// fakeCoordinator on why a dispatched one does not.
	_, err = raw.SetStatusForTest(t.Context(), st.ID, thread.StatusRunning, "", "", 0)
	require.NoError(t, err)

	tool := tools.NewAgentResultTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentResultParams{ID: st.ID})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "gamma")
	require.Contains(t, resp.Content, "running")
}

func TestThreadStatusTool_MissingID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewAgentResultTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentResultParams{})
	require.True(t, resp.IsError)
}

func TestThreadStatusTool_UnknownID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewAgentResultTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentResultParams{ID: "nope"})
	require.True(t, resp.IsError)
}

func TestThreadSendTool_ReactivatesThread(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "delta", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewAgentSendTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentSendParams{ID: st.ID, Message: "keep going"})
	require.False(t, resp.IsError)

	// The outcome the send itself reports, not the status a moment
	// later: the send decides then and there whether the thread took the
	// message as its next turn or queued it behind one in flight, and
	// that decision is what "reactivates" means. Reading the status
	// afterwards asked a different question - whether the turn it just
	// started was still going - and in these fakes a dispatch fails
	// immediately (see fakeCoordinator), so the answer was a coin flip.
	//
	// Either immediate wording counts, and which one comes back is not
	// this test's business: an idle thread whose workspace is still live
	// takes the message directly, one whose workspace has been released
	// is respawned first and reports a resume. Only "Queued" would mean
	// it did not reactivate.
	require.Regexp(t, "(?i)deliver", resp.Content)
	require.NotContains(t, resp.Content, "Queued",
		"reactivation means the message runs as the next turn, not behind one in flight")
}

func TestThreadRemoveTool_RefusesActiveWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr, _, raw := newTestThreadManagerRaw(t, repo)

	// No goal, so nothing is dispatched: Manager.Create sets the thread
	// up and leaves it idle. That matters twice over - the fakes here
	// never reach a coordinator, so a dispatched thread settles into a
	// terminal status on its own, and it does so asynchronously, which
	// would overwrite the status set below as readily as it used to
	// overwrite the running one the old test was hoping to catch.
	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "epsilon"})
	require.NoError(t, err)

	// The refusal is keyed on the thread being Running (Manager.Remove),
	// so the test puts it there rather than racing a dispatch to catch
	// it there. Whether the old test's tool call landed while the thread
	// was still running was a coin flip, and it came up wrong about one
	// run in twenty.
	_, err = raw.SetStatusForTest(t.Context(), st.ID, thread.StatusRunning, "", "", 0)
	require.NoError(t, err)

	tool := tools.NewThreadRemoveTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.ThreadRemoveParams{ID: st.ID})
	require.True(t, resp.IsError, "a running thread is not removable without force")
	require.Contains(t, resp.Content, "use force to remove")
}

func TestThreadRemoveTool_ForceRemoves(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "zeta", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewThreadRemoveTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.ThreadRemoveParams{ID: st.ID, Force: true})
	require.False(t, resp.IsError)

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err)
}

func TestThreadMergeTool_RefusesAThreadWithATurnInFlight(t *testing.T) {
	repo := initRepo(t)
	mgr, _, raw := newTestThreadManagerRaw(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "eta", MergePolicy: "manual"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(st.WorktreePath, "output.txt"), []byte("content\n"), 0o644))

	// Merging under a live turn commits whatever half-written state the
	// agent is holding — that file above — so the manager refuses, and
	// this pins that the tool surfaces the refusal rather than
	// swallowing it. The merge state machine itself is covered by
	// internal/thread's own tests.
	//
	// The running status is set, not dispatched: this used to create the
	// thread with a goal and trust that it would still be running by the
	// time the tool call landed, which it was about three runs in five.
	// See fakeCoordinator for why a dispatch here never gets that far.
	_, err = raw.SetStatusForTest(t.Context(), st.ID, thread.StatusRunning, "", "", 0)
	require.NoError(t, err)
	tool := tools.NewThreadMergeTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.ThreadMergeParams{ID: st.ID})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "active")
}

// TestThreadStatusTool_MissingThreadExplainsItself: asking about a thread
// that merged is now an ordinary thing to do, because merging removes it.
// The tool used to hand the model the store's own "sql: no rows in result
// set" — a database message with nothing to say about threads, which
// invites the conclusion that the work was lost and should be restarted.
func TestThreadStatusTool_MissingThreadExplainsItself(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)
	tool := tools.NewAgentResultTool(noTasks{}, mgr)

	resp := callTool(t, tool, tools.AgentResultParams{ID: "already-landed"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "already-landed", "name what was asked for")
	require.Contains(t, resp.Content, "removed once it merges", "and say what the absence means")
	require.NotContains(t, resp.Content, "sql:", "never the store's wording")
}

// TestAgentResultTool_TaskIDIsNotReadableAsAThread is the regression test
// for a scope escape. Threads and tasks are rows in one table, and
// thread.Manager.Get resolves either kind by id, so the delegation tools'
// "not a task of yours, try the thread manager" fallback happily returned
// somebody else's task and reported its goal, status and result - the
// scoping taskScope exists for, bypassed by a lookup order.
func TestAgentResultTool_TaskIDIsNotReadableAsAThread(t *testing.T) {
	repo := initRepo(t)
	mgr, store := newTestThreadManagerWithStore(t, repo)

	// A task belonging to a session that is not the caller's.
	task, err := store.Create(t.Context(), thread.CreateParams{
		Name:            "task-somebody-elses",
		Goal:            "secret goal",
		Kind:            thread.KindTask,
		ParentSessionID: "another-session",
	})
	require.NoError(t, err)

	tool := tools.NewAgentResultTool(noTasks{}, mgr)
	resp := callTool(t, tool, tools.AgentResultParams{ID: task.ID})
	require.True(t, resp.IsError, "a task the caller did not start is not readable")
	require.NotContains(t, resp.Content, "secret goal")
	require.Contains(t, resp.Content, "not among the tasks you started")
}
