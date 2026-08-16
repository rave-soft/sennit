package threadspawn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// git helpers (test-local copies of internal/thread's, since those are
// package-internal and this test now lives here).

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

// attachFakeSessions is the session-creation fake a thread's isolated
// (or the parent's, for tasks) App is wired with during attach tests.
type attachFakeSessions struct {
	session.Service
	mu             sync.Mutex
	n              int
	createdSession session.Session
}

func (f *attachFakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

func (f *attachFakeSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdSession = session.Session{ID: id, ParentSessionID: parentSessionID, Title: title}
	return f.createdSession, nil
}

// Get resolves the one session CreateTaskSession fabricated, for the
// completion-inbox path (see the internal/thread fakeSessions doc).
func (f *attachFakeSessions) Get(_ context.Context, id string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdSession.ID == id {
		return f.createdSession, nil
	}
	return session.Session{}, fmt.Errorf("threadspawn: attachFakeSessions: session %q not found", id)
}

// attachFakeCoordinator records dispatched runs; it never publishes
// RunComplete itself, so tests control when a run finishes.
type attachFakeCoordinator struct {
	agent.Coordinator

	mu         sync.Mutex
	runs       []attachFakeRun
	cancelAll  bool
	canceled   []string
	runErr     error
	delivered  []agent.TaskCompletion
	registered []agent.DelegationParent
}

type attachFakeRun struct {
	sessionID  string
	prompt     string
	runID      string
	delegation permission.DelegationRef
	origin     message.Origin
}

func (f *attachFakeCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, attachFakeRun{
		sessionID:  sessionID,
		prompt:     prompt,
		runID:      agent.RunIDFromContext(ctx),
		delegation: permission.DelegationFromContext(ctx),
		origin:     agent.PromptOriginFromContext(ctx),
	})
	return nil, f.runErr
}

func (f *attachFakeCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *attachFakeCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, attachFakeRun{
		sessionID:  sessionID,
		prompt:     prompt,
		runID:      agent.RunIDFromContext(ctx),
		delegation: permission.DelegationFromContext(ctx),
		origin:     agent.PromptOriginFromContext(ctx),
	})
	err := f.runErr
	f.mu.Unlock()
	return nil, err
}

func (f *attachFakeCoordinator) CancelAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelAll = true
}

func (f *attachFakeCoordinator) Cancel(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, sessionID)
}

func (f *attachFakeCoordinator) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// SetThreads / SetTasks / IsBusy are no-ops: App.Shutdown and Attach call
// them on the coordinator; the embedded nil agent.Coordinator would panic.
func (f *attachFakeCoordinator) SetThreads(tools.ThreadManager) {}
func (f *attachFakeCoordinator) SetTasks(tools.TaskManager)     {}
func (f *attachFakeCoordinator) IsBusy() bool                   { return false }

func (f *attachFakeCoordinator) RegisterDelegationParent(_ string, parent agent.DelegationParent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, parent)
}

func (f *attachFakeCoordinator) DeliverTaskCompletion(_ context.Context, _ string, completion agent.TaskCompletion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, completion)
}

func (f *attachFakeCoordinator) SendToParent(context.Context, string, string) error { return nil }

// attachTestHandle is the [thread.Handle] attachTestSpawner returns.
type attachTestHandle struct {
	id  string
	app *app.App
}

func (h *attachTestHandle) ID() string { return h.id }
func (h *attachTestHandle) App() *app.App {
	return h.app
}

func (h *attachTestHandle) Workspace() thread.Workspace {
	return &AppWorkspaceAdapter{App: h.app}
}

// attachTestSpawner spawns a real (network/db-free) app.App per thread,
// wired with the attach fakes, and keeps every spawned app/coordinator
// reachable by worktree path.
type attachTestSpawner struct {
	t *testing.T

	mu          sync.Mutex
	byPath      map[string]*attachTestHandle
	coordByPath map[string]*attachFakeCoordinator
}

func newAttachTestSpawner(t *testing.T) *attachTestSpawner {
	return &attachTestSpawner{
		t:           t,
		byPath:      make(map[string]*attachTestHandle),
		coordByPath: make(map[string]*attachFakeCoordinator),
	}
}

func (s *attachTestSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(&attachFakeSessions{})
	coord := &attachFakeCoordinator{}
	a.AgentCoordinator = coord

	h := &attachTestHandle{id: path, app: a}
	s.mu.Lock()
	s.byPath[path] = h
	s.coordByPath[path] = coord
	s.mu.Unlock()
	return h, nil
}

func (s *attachTestSpawner) Release(ctx context.Context, id string) error { return nil }

func (s *attachTestSpawner) coordFor(path string) *attachFakeCoordinator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coordByPath[path]
}
