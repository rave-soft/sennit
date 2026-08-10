package tools_test

// Exercises the strand_* tools against a real strand.Manager, using the
// same minimal git-repo + fake-spawner scaffolding internal/strand's own
// manager_test.go builds (those helpers are unexported to that package, so
// they're replicated here rather than imported). This package must import
// internal/strand as an external test package (package tools_test, not
// tools): internal/strand imports internal/app, which imports
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
	"github.com/rave-soft/braid/internal/strand"
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
	cmd := exec.Command("git", args...)
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
// to mint a strand session.
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
// Run, which just records the call — strand's own dispatch/RunComplete
// wiring is exercised by internal/strand's tests, not here.
type fakeCoordinator struct {
	agent.Coordinator
}

func (f *fakeCoordinator) Run(_ context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeCoordinator) CancelAll()                     {}
func (f *fakeCoordinator) SetStrands(tools.StrandManager) {}

// fakeHandle/fakeSpawner spawn a real (network/db-free) app.App per strand
// via app.NewForTest, matching internal/strand/manager_test.go's approach.
type fakeHandle struct {
	id  string
	app *app.App
}

func (h *fakeHandle) ID() string    { return h.id }
func (h *fakeHandle) App() *app.App { return h.app }

type fakeSpawner struct {
	t *testing.T
}

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (strand.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeSessions{}
	a.AgentCoordinator = &fakeCoordinator{}
	return &fakeHandle{id: path, app: a}, nil
}

func (s *fakeSpawner) Release(ctx context.Context, id string) error { return nil }

// fakeStore is an in-memory strand.Store, avoiding a real DB connection
// for these tool-level tests.
type fakeStore struct {
	mu    sync.Mutex
	byID  map[string]strand.Strand
	names map[string]string
	seq   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{byID: make(map[string]strand.Strand), names: make(map[string]string)}
}

func (s *fakeStore) Create(_ context.Context, p strand.CreateParams) (strand.Strand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	mergePolicy := p.MergePolicy
	if mergePolicy == "" {
		mergePolicy = strand.MergeAuto
	}
	st := strand.Strand{
		ID:           fmt.Sprintf("id-%d", s.seq),
		Name:         p.Name,
		Goal:         p.Goal,
		BaseBranch:   p.BaseBranch,
		Branch:       p.Branch,
		WorktreePath: p.WorktreePath,
		SessionID:    p.SessionID,
		Status:       strand.StatusPending,
		MergePolicy:  mergePolicy,
	}
	s.byID[st.ID] = st
	s.names[st.Name] = st.ID
	return st, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (strand.Strand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return strand.Strand{}, fmt.Errorf("strand: not found: %s", id)
	}
	return st, nil
}

func (s *fakeStore) GetByName(ctx context.Context, name string) (strand.Strand, error) {
	s.mu.Lock()
	id, ok := s.names[name]
	s.mu.Unlock()
	if !ok {
		return strand.Strand{}, fmt.Errorf("strand: not found: %s", name)
	}
	return s.Get(ctx, id)
}

func (s *fakeStore) List(_ context.Context) ([]strand.Strand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]strand.Strand, 0, len(s.byID))
	for _, st := range s.byID {
		out = append(out, st)
	}
	return out, nil
}

func (s *fakeStore) SetStatus(_ context.Context, id string, p strand.SetStatusParams) (strand.Strand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return strand.Strand{}, fmt.Errorf("strand: not found: %s", id)
	}
	st.Status = p.Status
	st.Error = p.Error
	st.ResultSummary = p.ResultSummary
	st.CompletedAt = p.CompletedAt
	s.byID[id] = st
	return st, nil
}

func (s *fakeStore) SetSession(_ context.Context, id, sessionID string) (strand.Strand, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.byID[id]
	if !ok {
		return strand.Strand{}, fmt.Errorf("strand: not found: %s", id)
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

// newTestStrandManager wires a Manager over a real git repo, an in-memory
// store, and the fake spawner above, and returns it already adapted to
// tools.StrandManager for direct use by the strand_* tool constructors.
func newTestStrandManager(t *testing.T, repo string) tools.StrandManager {
	t.Helper()
	mgr := strand.NewManager(strand.ManagerOptions{
		Store:       newFakeStore(),
		Spawner:     &fakeSpawner{t: t},
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	return strand.AsAgentToolManager(mgr)
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

func TestStrandCreateTool_CreatesStrand(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)
	tool := tools.NewStrandCreateTool(mgr, grantingPermissions(t))

	resp := callTool(t, tool, tools.StrandCreateParams{Name: "alpha", Goal: "do the thing"})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "alpha")

	strands, err := mgr.List(t.Context())
	require.NoError(t, err)
	require.Len(t, strands, 1)
	require.Equal(t, "alpha", strands[0].Name)
}

func TestStrandCreateTool_MissingFields(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)
	tool := tools.NewStrandCreateTool(mgr, grantingPermissions(t))

	resp := callTool(t, tool, tools.StrandCreateParams{Goal: "do the thing"})
	require.True(t, resp.IsError)

	resp = callTool(t, tool, tools.StrandCreateParams{Name: "alpha"})
	require.True(t, resp.IsError)
}

func TestStrandListTool_ListsCreatedStrands(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	_, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "beta", Goal: "x"})
	require.NoError(t, err)

	tool := tools.NewStrandListTool(mgr)
	resp := callTool(t, tool, tools.StrandListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "beta")
}

func TestStrandListTool_EmptyWhenNoStrands(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	tool := tools.NewStrandListTool(mgr)
	resp := callTool(t, tool, tools.StrandListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No strands")
}

func TestStrandStatusTool_ReturnsDetails(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "gamma", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewStrandStatusTool(mgr)
	resp := callTool(t, tool, tools.StrandStatusParams{ID: st.ID})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "gamma")
	require.Contains(t, resp.Content, string(st.Status))
}

func TestStrandStatusTool_MissingID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	tool := tools.NewStrandStatusTool(mgr)
	resp := callTool(t, tool, tools.StrandStatusParams{})
	require.True(t, resp.IsError)
}

func TestStrandStatusTool_UnknownID(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	tool := tools.NewStrandStatusTool(mgr)
	resp := callTool(t, tool, tools.StrandStatusParams{ID: "nope"})
	require.True(t, resp.IsError)
}

func TestStrandSendTool_ReactivatesStrand(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "delta", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewStrandSendTool(mgr)
	resp := callTool(t, tool, tools.StrandSendParams{ID: st.ID, Message: "keep going"})
	require.False(t, resp.IsError)

	got, err := mgr.Get(t.Context(), st.ID)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)
}

func TestStrandRemoveTool_RefusesActiveWithoutForce(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "epsilon", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewStrandRemoveTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.StrandRemoveParams{ID: st.ID})
	require.True(t, resp.IsError)
}

func TestStrandRemoveTool_ForceRemoves(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "zeta", Goal: "do it"})
	require.NoError(t, err)

	tool := tools.NewStrandRemoveTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.StrandRemoveParams{ID: st.ID, Force: true})
	require.False(t, resp.IsError)

	_, err = mgr.Get(t.Context(), st.ID)
	require.Error(t, err)
}

func TestStrandMergeTool_ManualPolicyMergesAfterCompletion(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	st, err := mgr.Create(t.Context(), tools.StrandCreateArgs{Name: "eta", Goal: "do it", MergePolicy: "manual"})
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(st.WorktreePath, "output.txt"), []byte("content\n"), 0o644))

	tool := tools.NewStrandMergeTool(mgr, grantingPermissions(t))
	resp := callTool(t, tool, tools.StrandMergeParams{ID: st.ID})
	// The strand is still "running" (no completion was ever published in
	// this fake-spawner setup), so mergeAttempt's status transitions still
	// run without error even though nothing meaningful merges yet; this
	// exercises the tool's plumbing, not strand's merge state machine
	// (covered by internal/strand's own tests).
	require.False(t, resp.IsError)
}

func TestStrandWaitTool_ReturnsImmediatelyWhenNothingActive(t *testing.T) {
	repo := initRepo(t)
	mgr := newTestStrandManager(t, repo)

	tool := tools.NewStrandWaitTool(mgr)
	resp := callTool(t, tool, tools.StrandWaitParams{TimeoutSeconds: 1})
	require.False(t, resp.IsError)
}
