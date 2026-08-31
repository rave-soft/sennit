package appws

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// releaseGatedCoordinator is a minimal agent.Coordinator whose Run blocks
// on release rather than on ctx.Done(), signalling entry via entered
// first. Unlike streamingCoordinator (agent_run_stream_abandoned_consumer_test.go),
// Run here does not race the fan-in goroutine's own ctx.Done() case: it
// keeps `done` empty until the test explicitly releases it, well after
// the assertions below have run, so the only way the fan-in goroutine in
// AgentRunStream can produce a terminal event during the test is its own
// `case <-ctx.Done()` branch. That determinism is the point — this test
// exists to catch AgentRunStream discarding that exact terminal event.
type releaseGatedCoordinator struct {
	agent.Coordinator
	entered chan struct{}
	release chan struct{}
}

func (c *releaseGatedCoordinator) UpdateModels(context.Context) error { return nil }

func (c *releaseGatedCoordinator) Run(_ context.Context, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	close(c.entered)
	<-c.release
	return nil, context.Canceled
}

// TestAppWorkspace_AgentRunStream_CtxCancelAlwaysDeliversTerminalEvent is
// the regression test for the bug described in AgentRunStream's fan-in
// goroutine: its `case <-ctx.Done()` branch used to call send(ev), and
// send races `out <- ev` against `<-ctx.Done()` — but ctx is already
// done by construction at that call site, so both cases were always
// ready and Go picked between them at random. About half the time the
// terminal event was silently discarded, close(out) still ran (deferred,
// unconditionally), and a consumer draining the channel saw a clean
// close with no terminal event at all — indistinguishable, at the
// channel level, from the turn finishing on its own. `sennit run` built
// exactly that mistake into its exit code (see internal/cmd/run.go).
//
// It loops 50 times, cancelling ctx mid-run on each iteration and
// requiring a terminal event with a non-nil Err before the channel
// closes. A single iteration would pass or fail on a coin flip against
// the old code; 50 makes a false pass astronomically unlikely (roughly
// 1 in 2^50) while still running in well under a second against the
// fixed code, since the fix delivers deterministically rather than
// probabilistically.
func TestAppWorkspace_AgentRunStream_CtxCancelAlwaysDeliversTerminalEvent(t *testing.T) {
	sessions, messages := newRealSessionAgentEnv(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.MCP = mcp.NewRegistry() // AgentRunStream calls WaitForInit unconditionally; NewForTest leaves it nil.
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)

	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	const iterations = 50
	for i := 0; i < iterations; i++ {
		coord := &releaseGatedCoordinator{entered: make(chan struct{}), release: make(chan struct{})}
		a.SetAgentCoordinatorForTest(coord)

		ctx, cancel := context.WithCancel(t.Context())

		out, err := aw.AgentRunStream(ctx, sess.ID, "hello")
		require.NoError(t, err)

		select {
		case <-coord.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: coordinator.Run never entered", i)
		}

		cancel()

		var (
			gotTerminal bool
			terminalErr error
		)
	drain:
		for {
			select {
			case ev, ok := <-out:
				if !ok {
					break drain
				}
				if ev.Done {
					gotTerminal = true
					terminalErr = ev.Err
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d: never observed the channel close", i)
			}
		}

		require.True(t, gotTerminal, "iteration %d: channel closed without a terminal (Done) event", i)
		require.Error(t, terminalErr, "iteration %d: terminal event must carry ctx's cancellation error", i)

		close(coord.release)
		cancel()
	}
}

func TestAppWorkspace_AgentRunStream_InternalCancellationIsTerminalError(t *testing.T) {
	sessions, messages := newRealSessionAgentEnv(t)
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.MCP = mcp.NewRegistry()
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)
	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	coord := &releaseGatedCoordinator{entered: make(chan struct{}), release: make(chan struct{})}
	a.SetAgentCoordinatorForTest(coord)
	out, err := aw.AgentRunStream(t.Context(), sess.ID, "hello")
	require.NoError(t, err)
	select {
	case <-coord.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator.Run never entered")
	}
	close(coord.release) // coordinator returns context.Canceled, caller remains live.

	var terminal error
	for ev := range out {
		if ev.Done {
			terminal = ev.Err
		}
	}
	require.ErrorIs(t, terminal, context.Canceled)
	require.ErrorContains(t, terminal, "agent processing failed")
}

// SetLiveSession is inert: AgentRunStream reports the run's session
// through App.ReportCurrentSession, and this double only has to answer it.
func (c *releaseGatedCoordinator) SetLiveSession(string) {}
