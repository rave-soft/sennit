package backend

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/stretchr/testify/require"
)

// blockingCoordinator is a minimal agent.Coordinator whose RunAccepted
// blocks until release is closed. It records that RunAccepted was
// entered so tests can observe the dispatched goroutine. Every other
// method returns a zero value.
type blockingCoordinator struct {
	entered  chan struct{}
	release  chan struct{}
	runCount atomic.Int32
}

func newBlockingCoordinator() *blockingCoordinator {
	return &blockingCoordinator{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (c *blockingCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (c *blockingCoordinator) RunAccepted(ctx context.Context, accept *agent.AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.runCount.Add(1)
	select {
	case c.entered <- struct{}{}:
	default:
	}
	<-c.release
	return nil, nil
}

func (c *blockingCoordinator) Steer(ctx context.Context, call agent.SessionAgentCall) (agent.SteerOutcome, *fantasy.AgentResult, error) {
	return agent.SteerRan, nil, nil
}

func (c *blockingCoordinator) BeginAccepted(sessionID string) *agent.AcceptedRun  { return nil }
func (c *blockingCoordinator) Cancel(string)                                      {}
func (c *blockingCoordinator) CancelAll()                                         {}
func (c *blockingCoordinator) IsBusy() bool                                       { return false }
func (c *blockingCoordinator) IsSessionBusy(string) bool                          { return false }
func (c *blockingCoordinator) QueuedPrompts(string) int                           { return 0 }
func (c *blockingCoordinator) QueuedPromptsList(string) []string                  { return nil }
func (c *blockingCoordinator) ClearQueue(string)                                  {}
func (c *blockingCoordinator) Summarize(context.Context, string) error            { return nil }
func (c *blockingCoordinator) Model() agent.Model                                 { return agent.Model{} }
func (c *blockingCoordinator) UpdateModels(context.Context) error                 { return nil }
func (c *blockingCoordinator) GenerateTitle(context.Context, string, string)      {}
func (c *blockingCoordinator) SetThreads(tools.ThreadManager)                     {}
func (c *blockingCoordinator) SetTasks(tools.TaskManager)                         {}
func (c *blockingCoordinator) DeliverTaskCompletion(string, agent.TaskCompletion) {}
func (c *blockingCoordinator) RefreshSkills([]*skills.Skill, []*skills.Skill)     {}

// insertAgentWorkspace installs a synthetic workspace with the given
// coordinator (or none) and a workspace run context, mirroring the
// fields CreateWorkspace initializes.
func insertAgentWorkspace(t *testing.T, b *Backend, coord agent.Coordinator) *Workspace {
	t.Helper()
	ws := &Workspace{
		ID:           uuid.New().String(),
		Path:         t.TempDir(),
		resolvedPath: t.TempDir(),
		clients:      make(map[string]*clientState),
		shutdownFn:   func() {},
	}
	ws.App = &app.App{AgentCoordinator: coord}
	ws.ctx, ws.cancel = context.WithCancel(b.ctx)
	ws.dispatcher = app.NewAgentDispatcher(ws.ctx, func() agent.Coordinator { return coord }, ws.AgentNotifications(), ws.RunCompletions())
	b.mu.Lock()
	b.workspaces.Set(ws.ID, ws)
	b.pathIndex[ws.resolvedPath] = ws.ID
	b.mu.Unlock()
	return ws
}

func TestSendMessage_WorkspaceNotFound(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	err := b.SendMessage("nope", proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

func TestSendMessage_AgentNotInitialized(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, nil)
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.ErrorIs(t, err, ErrAgentNotInitialized)
}

// TestSendMessage_NoPanicWhenCoordinatorNeverInitialized is a
// regression test for a workspace built through the real
// Backend.CreateWorkspace path whose Backend.InitAgent is never called
// (e.g. no provider configured yet). The dispatcher used to be built
// only inside InitAgent, so this exact path dereferenced a nil
// Workspace.dispatcher in SendMessage and panicked; the dispatcher is
// now built unconditionally in createWorkspace, so this must return a
// clean ErrAgentNotInitialized instead.
func TestSendMessage_NoPanicWhenCoordinatorNeverInitialized(t *testing.T) {
	// Isolate config.Init from the host so nothing on this machine (a
	// real API key, a stored credentials.json) accidentally configures
	// a provider and defeats the test's premise.
	hostHome := t.TempDir()
	t.Setenv("HOME", hostHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(hostHome, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(hostHome, ".local", "share"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(hostHome, ".cache"))

	wd := t.TempDir()
	srvCfg, err := config.Init(wd, "", false)
	require.NoError(t, err)
	b := New(t.Context(), srvCfg, nil)

	clientID := uuid.New().String()
	ws, _, err := b.CreateWorkspace(proto.Workspace{
		ClientID: clientID,
		Path:     wd,
		DataDir:  filepath.Join(wd, ".braid"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.DeleteWorkspace(ws.ID, clientID) })

	// The test's premise: nothing configured a provider, so
	// app.New never called InitCoderAgent and Backend.InitAgent was
	// never invoked either. If this assumption stops holding the test
	// would silently stop exercising the nil-coordinator path.
	require.Nil(t, ws.AgentCoordinator)

	require.NotPanics(t, func() {
		err = b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	})
	require.ErrorIs(t, err, ErrAgentNotInitialized)
}

func TestSendMessage_EmptyPrompt(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: ""})
	require.ErrorIs(t, err, agent.ErrEmptyPrompt)
}

func TestSendMessage_SessionMissing(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "", Prompt: "hi"})
	require.ErrorIs(t, err, agent.ErrSessionMissing)
}

func TestSendMessage_WorkspaceClosing(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	ws := insertAgentWorkspace(t, b, newBlockingCoordinator())
	ws.dispatcher.MarkClosing()
	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.ErrorIs(t, err, ErrWorkspaceClosing)
}

// TestSendMessage_SuccessIncrementsRunWG asserts the happy path returns
// nil synchronously and dispatches a tracked goroutine: while
// RunAccepted blocks, dispatcher.Wait must not complete (the ticket is
// outstanding); after release it drains.
func TestSendMessage_SuccessIncrementsRunWG(t *testing.T) {
	t.Parallel()
	b, _ := newTestBackend(t)
	coord := newBlockingCoordinator()
	ws := insertAgentWorkspace(t, b, coord)

	err := b.SendMessage(ws.ID, proto.AgentMessage{SessionID: "S1", Prompt: "hi"})
	require.NoError(t, err)

	select {
	case <-coord.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched goroutine never entered RunAccepted")
	}
	require.Equal(t, int32(1), coord.runCount.Load())

	waited := make(chan struct{})
	go func() {
		ws.dispatcher.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("dispatcher.Wait completed while the run was still in flight; ticket was not added")
	case <-time.After(100 * time.Millisecond):
	}

	close(coord.release)

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher.Wait did not complete after the run returned")
	}
}
