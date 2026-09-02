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
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// streamingCoordinator is a minimal agent.Coordinator for AgentRunStream
// whose Run publishes one message event through publish, then blocks
// until the test's context is canceled, at which point it returns
// ctx.Err(). This lets a test hold the run "in flight" long enough to
// abandon the consumer before the turn completes.
type streamingCoordinator struct {
	agent.Coordinator
	publish func()
}

func (c *streamingCoordinator) UpdateModels(context.Context) error { return nil }

func (c *streamingCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	c.publish()
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAppWorkspace_AgentRunStream_AbandonedConsumerDoesNotLeakGoroutine is
// the regression test for AgentRunStream's fan-in goroutine blocking
// forever on an unbuffered send once the consumer stops reading: every
// `out <- AgentRunEvent{...}` used to send unconditionally, so a consumer
// that read one event and then walked away left the goroutine parked on
// its next send with no way to observe ctx.Done — and with it, the
// deferred cancel() and close(out) never ran, leaking the goroutine, the
// context, and the message-events subscription forever.
//
// It deliberately does not probe for the leak by reading from `out`
// again: doing so would itself supply the missing receiver and rescue a
// buggy goroutine's single pending send, masking the very bug this test
// exists to catch. Instead it stops reading for good after the first
// event and uses goleak to confirm the goroutines this call started are
// actually gone.
func TestAppWorkspace_AgentRunStream_AbandonedConsumerDoesNotLeakGoroutine(t *testing.T) {
	sessions, messages := newRealSessionAgentEnv(t)

	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.MCP = mcp.NewRegistry() // AgentRunStream calls WaitForInit unconditionally; NewForTest leaves it nil.
	a.SetSessionsForTest(sessions)
	a.SetMessagesForTest(messages)

	sess, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// published carries the Create error (nil on success) rather than
	// using require inside the coordinator goroutine, whose FailNow
	// would be unsafe off the test's own goroutine.
	published := make(chan error, 1)
	a.SetAgentCoordinatorForTest(&streamingCoordinator{
		publish: func() {
			// A fresh, unfinished assistant message reports
			// Working{Phase: PhaseWorking} ("Working"), which differs
			// from the fan-in goroutine's initial lastStatus (""), so
			// creating it produces exactly one Status event — enough to
			// exercise the abandoned send without also racing a
			// TextDelta out of the same message.
			_, err := messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
				Role: message.Assistant,
			})
			published <- err
		},
	})

	ignoreBaseline := goleak.IgnoreCurrent()

	ctx, cancel := context.WithCancel(t.Context())
	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	out, err := aw.AgentRunStream(ctx, sess.ID, "hello", workspace.AgentRunOptions{AutoApprovePermissions: true})
	require.NoError(t, err)

	// Read exactly one event, then stop reading altogether — the
	// abandoned-consumer scenario. No further receive on out happens
	// anywhere else in this test.
	select {
	case _, ok := <-out:
		require.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("never received the first event")
	}

	select {
	case err := <-published:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("coordinator never published")
	}

	cancel()

	// The fix's own goroutines (the fan-in loop, the Run wrapper, and
	// the message-events subscription's fan-out) must all wind down on
	// their own now that ctx is canceled, with nobody left reading out.
	deadline := time.Now().Add(2 * time.Second)
	var leakErr error
	for time.Now().Before(deadline) {
		leakErr = goleak.Find(ignoreBaseline)
		if leakErr == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("an abandoned consumer must not leak AgentRunStream's goroutines: %v", leakErr)
}

// SetLiveSession is inert: AgentRunStream reports the run's session
// through App.ReportCurrentSession, and this double only has to answer.
func (s *streamingCoordinator) SetLiveSession(string) {}
