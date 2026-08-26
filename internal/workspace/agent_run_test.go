package workspace

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// blockingAgentRunCoordinator is a minimal agent.Coordinator whose
// RunAccepted blocks until release is closed, signalling entry via
// entered first. It exists to make AppWorkspace.AgentRun's accept-time
// return observable rather than assumed from timing.
type blockingAgentRunCoordinator struct {
	agent.Coordinator
	entered chan struct{}
	release chan struct{}
}

func (c *blockingAgentRunCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (c *blockingAgentRunCoordinator) RunAccepted(_ context.Context, _ *agent.AcceptedRun, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	close(c.entered)
	<-c.release
	return nil, nil
}

// Run also blocks the same way as RunAccepted, so this fake behaves
// identically whether AgentRun calls the dispatcher (RunAccepted) or,
// were it ever reverted, the coordinator directly (Run): the ordering
// test below would then time out waiting for AgentRun to return, not
// panic on an unimplemented method.
func (c *blockingAgentRunCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.RunAccepted(ctx, nil, sessionID, prompt, attachments...)
}

func (c *blockingAgentRunCoordinator) CancelAll() {}

func (c *blockingAgentRunCoordinator) Cancel(sessionID string) {}

// IsBusy is queried by App.Shutdown's agent-work phase right after
// CancelAll, independently of whether anything is actually
// dispatched.
func (c *blockingAgentRunCoordinator) IsBusy() bool { return false }

// TestAppWorkspace_AgentRun_ReturnsBeforeTurnCompletes proves
// AppWorkspace.AgentRun dispatches fire-and-forget: it must return once
// the prompt is accepted, not once the LLM turn finishes. The fake
// coordinator blocks inside RunAccepted until the test releases it, so
// "AgentRun returned"
// is observed strictly before "the turn completed" — a timer-based
// version of this test could pass even if AgentRun still blocked for
// the whole turn, as long as the turn happened to finish quickly; this
// one cannot, because nothing lets RunAccepted return until release is
// closed, which only happens after the assertion below.
func TestAppWorkspace_AgentRun_ReturnsBeforeTurnCompletes(t *testing.T) {
	t.Parallel()

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	entered := make(chan struct{})
	release := make(chan struct{})
	a.AgentCoordinator = &blockingAgentRunCoordinator{entered: entered, release: release}

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	done := make(chan error, 1)
	go func() {
		done <- aw.AgentRun(t.Context(), "S1", "hello")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched run never entered RunAccepted")
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AgentRun did not return once the run was accepted")
	}

	// The run is still blocked in RunAccepted at this point (release
	// has not been closed yet); let it finish so the goroutine does
	// not leak past the test.
	close(release)
}

// TestAppWorkspace_AgentRun_ValidationErrorIsSynchronous asserts that
// structural refusal errors (here, agent.ValidateCall's empty-prompt
// check) are still returned directly from AgentRun rather than only
// reaching observers as a notification.
func TestAppWorkspace_AgentRun_ValidationErrorIsSynchronous(t *testing.T) {
	t.Parallel()

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.AgentCoordinator = &blockingAgentRunCoordinator{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	err := aw.AgentRun(t.Context(), "S1", "")
	require.ErrorIs(t, err, agent.ErrEmptyPrompt)
}

// TestAppWorkspace_AgentRun_UninitializedCoordinatorIsSynchronous
// asserts that an App with no coordinator at all (the unconfigured
// project case) still refuses synchronously instead of dispatching.
func TestAppWorkspace_AgentRun_UninitializedCoordinatorIsSynchronous(t *testing.T) {
	t.Parallel()

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	err := aw.AgentRun(t.Context(), "S1", "hello")
	require.ErrorIs(t, err, app.ErrCoordinatorNotInitialized)
}

// TestAppWorkspace_Shutdown_JoinsRunDispatchedViaAgentRun is stage 4's
// shutdown-ordering case, checked specifically through the workspace
// layer. The substantive guarantee — App.Shutdown must join every
// dispatcher-issued run before MCP/DB cleanup — is already proven at
// the App level by
// TestApp_Shutdown_AgentDispatcherJoinedBeforeDBAndMCP
// (internal/app/app_agent_dispatch_test.go), which dispatches directly
// through a.AgentDispatcher().Send and is not repeated here. What is
// genuinely additional at this layer is that a run dispatched through
// AppWorkspace.AgentRun specifically reaches that same dispatcher
// instance, rather than some other path Shutdown does not know to
// join.
//
// It would fail if AppWorkspace.AgentRun ever stopped calling
// w.app.AgentDispatcher().Send — e.g. reverted to a direct
// AgentCoordinator.Run call bypassing the dispatcher, or wired against
// a second, unjoined dispatcher instance: Shutdown would then return
// while the run dispatched here is still blocked in RunAccepted,
// exactly as it would if the App-level test's Wait call were deleted
// (verified for that test in the previous step's report).
func TestAppWorkspace_Shutdown_JoinsRunDispatchedViaAgentRun(t *testing.T) {
	t.Parallel()

	a := app.NewForTest(t.Context())
	entered := make(chan struct{})
	release := make(chan struct{})
	a.AgentCoordinator = &blockingAgentRunCoordinator{entered: entered, release: release}

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	runDone := make(chan error, 1)
	go func() {
		runDone <- aw.AgentRun(t.Context(), "S1", "hello")
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatched run never entered RunAccepted")
	}
	select {
	case err := <-runDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("AgentRun did not return once the run was accepted")
	}

	shutdownDone := make(chan struct{})
	go func() {
		a.Shutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		t.Fatal("Shutdown completed while a run dispatched via AppWorkspace.AgentRun was still blocked in RunAccepted")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not complete after the blocked run was released")
	}
}
