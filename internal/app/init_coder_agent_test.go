package app

import (
	"context"
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

// withFakeNewCoordinator substitutes the newCoordinator seam with build for
// the duration of the test, restoring the real agent.NewCoordinator
// afterwards. Not safe for t.Parallel() callers, since the seam is a
// package-level variable.
func withFakeNewCoordinator(t *testing.T, build func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error)) {
	t.Helper()
	original := newCoordinator
	newCoordinator = build
	t.Cleanup(func() { newCoordinator = original })
}

// recordingCoordinator is a minimal agent.Coordinator that records the
// delegation tool adapters it was constructed with and how many times it
// was closed.
type recordingCoordinator struct {
	agent.Coordinator

	threads    tools.ThreadManager
	tasks      tools.TaskManager
	closeCalls int
}

func (c *recordingCoordinator) Close(context.Context) error {
	c.closeCalls++
	return nil
}

func (c *recordingCoordinator) IsBusy() bool { return false }

// appWithCoderAgentConfigured returns a NewForTest App whose config has a
// non-empty AgentCoder entry, which is all initCoderAgent's own
// "coder agent configuration is missing" guard requires before it reaches
// the (faked) coordinator constructor.
func appWithCoderAgentConfigured(t *testing.T) *App {
	t.Helper()
	a := NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	cfg := &config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents:    map[string]config.Agent{config.AgentCoder: {ID: config.AgentCoder}},
	}
	store := configtest.NewStore(t, cfg, configtest.WithWorkingDir(t.TempDir()))
	a.SetConfigForTest(store)
	return a
}

// fakeThreadManagerAdapter and fakeTaskManagerAdapter are distinct,
// otherwise-empty implementations of the tool adapter interfaces, used to
// tell "some adapter" apart from "no adapter" by identity.
type fakeThreadManagerAdapter struct{ tools.ThreadManager }

type fakeTaskManagerAdapter struct{ tools.TaskManager }

// TestInitCoderAgent_RebuildReappliesPublishedDelegationTools is the
// regression test for the bug this fixes: InitCoderAgentNonInteractive
// (as `sennit run` calls it, after InitCoderAgent and threadspawn.Attach
// already ran) used to build a brand new coordinator with no Threads/
// Tasks in its options at all, silently dropping the thread_*/task tools
// Attach had already published. It must now recover them from the
// delegation snapshot and hand them to the new coordinator at
// construction time.
//
// Without the fix (initCoderAgent building CoordinatorOptions with no
// Threads/Tasks fields), this test fails: gotThreads/gotTasks come back
// nil even though SetDelegationManagers published non-nil adapters.
func TestInitCoderAgent_RebuildReappliesPublishedDelegationTools(t *testing.T) {
	a := appWithCoderAgentConfigured(t)

	threadTools := fakeThreadManagerAdapter{}
	taskTools := fakeTaskManagerAdapter{}
	a.SetDelegationManagers(nil, nil, threadTools, taskTools)

	var gotThreads tools.ThreadManager
	var gotTasks tools.TaskManager
	withFakeNewCoordinator(t, func(_ context.Context, opts agent.CoordinatorOptions) (agent.Coordinator, error) {
		gotThreads = opts.Threads
		gotTasks = opts.Tasks
		return &recordingCoordinator{threads: opts.Threads, tasks: opts.Tasks}, nil
	})

	require.NoError(t, a.InitCoderAgentNonInteractive(t.Context()))
	require.Equal(t, threadTools, gotThreads)
	require.Equal(t, taskTools, gotTasks)
}

// TestInitCoderAgent_ClosesReplacedCoordinator proves a rebuild closes the
// coordinator it replaces, fixing the leak described in the bug report:
// the old coordinator's Close (which stops its background readiness work
// and the git/MCP subprocesses that work may spawn) used to never run
// because nothing but App.Shutdown ever called it, and initCoderAgent's
// second call overwrote the field before Shutdown got a chance.
func TestInitCoderAgent_ClosesReplacedCoordinator(t *testing.T) {
	a := appWithCoderAgentConfigured(t)

	old := &recordingCoordinator{}
	a.SetAgentCoordinatorForTest(old)

	withFakeNewCoordinator(t, func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error) {
		return &recordingCoordinator{}, nil
	})

	require.NoError(t, a.InitCoderAgentNonInteractive(t.Context()))
	require.Equal(t, 1, old.closeCalls, "the replaced coordinator must be closed exactly once")
	require.NotSame(t, old, a.Coordinator(), "the field must now hold the newly built coordinator")
}

// TestInitCoderAgent_FailedRebuildLeavesExistingCoordinatorInPlace proves
// the ordering half of the fix: a NewCoordinator failure must not
// overwrite the coordinator with the error's nil (which is what the
// original `app.AgentCoordinator, err = agent.NewCoordinator(...)` did),
// and must not close the coordinator that is still in use.
func TestInitCoderAgent_FailedRebuildLeavesExistingCoordinatorInPlace(t *testing.T) {
	a := appWithCoderAgentConfigured(t)

	old := &recordingCoordinator{}
	a.SetAgentCoordinatorForTest(old)

	buildErr := errors.New("boom")
	withFakeNewCoordinator(t, func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error) {
		return nil, buildErr
	})

	err := a.InitCoderAgentNonInteractive(t.Context())
	require.ErrorIs(t, err, buildErr)
	require.Same(t, old, a.Coordinator(), "a failed rebuild must leave the existing coordinator in place")
	require.Zero(t, old.closeCalls, "the still-in-use coordinator must not be closed")
}
