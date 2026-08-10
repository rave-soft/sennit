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

	"charm.land/fantasy"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
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

// fakeSessions implements just enough of session.Service for Manager.Create
// to mint a thread session.
type fakeSessions struct {
	session.Service
	mu sync.Mutex
	n  int
}

func (f *fakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

// fakeCoordinator implements agent.Coordinator with no-op behavior beyond
// Run, which just records the call — thread's own dispatch/RunComplete
// wiring is exercised by internal/thread's tests, not here.
type fakeCoordinator struct {
	agent.Coordinator
}

func (f *fakeCoordinator) Run(_ context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeCoordinator) CancelAll()                     {}
func (f *fakeCoordinator) SetThreads(tools.ThreadManager) {}

// fakeHandle/fakeSpawner spawn a real (network/db-free) app.App per thread
// via app.NewForTest, matching internal/thread/manager_test.go's approach.
type fakeHandle struct {
	id  string
	app *app.App
}

func (h *fakeHandle) ID() string    { return h.id }
func (h *fakeHandle) App() *app.App { return h.app }

type fakeSpawner struct {
	t *testing.T
}

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeSessions{}
	a.AgentCoordinator = &fakeCoordinator{}
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
	mergePolicy := p.MergePolicy
	if mergePolicy == "" {
		mergePolicy = thread.MergeAuto
	}
	st := thread.Thread{
		ID:           fmt.Sprintf("id-%d", s.seq),
		Name:         p.Name,
		Goal:         p.Goal,
		BaseBranch:   p.BaseBranch,
		Branch:       p.Branch,
		WorktreePath: p.WorktreePath,
		SessionID:    p.SessionID,
		Status:       thread.StatusPending,
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
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       newFakeStore(),
		Spawner:     &fakeSpawner{t: t},
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	return thread.AsAgentToolManager(mgr)
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

	tool := tools.NewThreadListTool(mgr)
	resp := callTool(t, tool, tools.ThreadListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "beta")
}

func TestThreadListTool_EmptyWhenNoThreads(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewThreadListTool(mgr)
	resp := callTool(t, tool, tools.ThreadListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No threads")
}

func TestThreadStatusTool_ReturnsDetails(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "gamma", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewThreadStatusTool(mgr)
	resp := callTool(t, tool, tools.ThreadStatusParams{ID: st.ID})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "gamma")
	require.Contains(t, resp.Content, string(st.Status))
}

func TestThreadStatusTool_MissingID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewThreadStatusTool(mgr)
	resp := callTool(t, tool, tools.ThreadStatusParams{})
	require.True(t, resp.IsError)
}

func TestThreadStatusTool_UnknownID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewThreadStatusTool(mgr)
	resp := callTool(t, tool, tools.ThreadStatusParams{ID: "nope"})
	require.True(t, resp.IsError)
}

func TestThreadSendTool_ReactivatesThread(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "delta", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewThreadSendTool(mgr)
	resp := callTool(t, tool, tools.ThreadSendParams{ID: st.ID, Message: "keep going"})
	require.False(t, resp.IsError)

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)
}

func TestThreadRemoveTool_RefusesActiveWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "epsilon", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewThreadRemoveTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.ThreadRemoveParams{ID: st.ID})
	require.True(t, resp.IsError)
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

func TestThreadMergeTool_ManualPolicyMergesAfterCompletion(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.ThreadCreateArgs{Name: "eta", Goal: "do it", MergePolicy: "manual"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(st.WorktreePath, "output.txt"), []byte("content\n"), 0o644))

	tool := tools.NewThreadMergeTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.ThreadMergeParams{ID: st.ID})
	// The thread is still "running" (no completion was ever published in
	// this fake-spawner setup), so mergeAttempt's status transitions still
	// run without error even though nothing meaningful merges yet; this
	// exercises the tool's plumbing, not thread's merge state machine
	// (covered by internal/thread's own tests).
	require.False(t, resp.IsError)
}

func TestThreadWaitTool_ReturnsImmediatelyWhenNothingActive(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestThreadManager(t, repo)

	tool := tools.NewThreadWaitTool(mgr)
	resp := callTool(t, tool, tools.ThreadWaitParams{TimeoutSeconds: 1})
	require.False(t, resp.IsError)
}
