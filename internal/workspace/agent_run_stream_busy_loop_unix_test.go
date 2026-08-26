//go:build unix

// The busy-loop assertion measures consumed CPU time via
// syscall.Getrusage, which exists only on unix. The loop it guards is
// platform-independent, so covering it here is enough.

package workspace

import (
	"context"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// blockingStreamCoordinator is a minimal agent.Coordinator for
// AgentRunStream: UpdateModels succeeds immediately, and Run blocks until
// release is closed, signalling entry via entered first — the same shape
// as blockingAgentRunCoordinator above, but implementing the methods
// AgentRunStream itself calls directly rather than through the dispatcher.
type blockingStreamCoordinator struct {
	agent.Coordinator
	entered chan struct{}
	release chan struct{}
}

func (c *blockingStreamCoordinator) UpdateModels(context.Context) error { return nil }

func (c *blockingStreamCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	close(c.entered)
	<-c.release
	return &fantasy.AgentResult{}, nil
}

// controlledMessagesService is a messagestore.Service double whose Subscribe
// always returns the same test-controlled channel, so the test can close
// it independently of AgentRunStream's own context — the scenario the
// broker's real Shutdown() produces, but without needing a second app or
// racing the derived ctx's own cancellation.
type controlledMessagesService struct {
	messagestore.Service
	ch chan pubsub.Event[message.Message]
}

func (m *controlledMessagesService) Subscribe(context.Context) <-chan pubsub.Event[message.Message] {
	return m.ch
}

// TestAppWorkspace_AgentRunStream_ClosedMessageChannelDoesNotBusyLoop is
// the regression test for the `case ev := <-messageEvents:` read in
// AgentRunStream's fan-in goroutine: it used to ignore the received-ok
// result, so once the broker closed messageEvents (its Shutdown, or a
// process-wide reset) the loop spun on that always-ready closed channel
// consuming CPU at 100% until the run itself finished, instead of parking
// until there was real work. Coordinator.Run is held open the whole test
// so nothing else (done firing, or the derived ctx being cancelled from
// outside) can make the loop exit on its own — CPU usage during that
// window is the only thing that can distinguish the two versions.
func TestAppWorkspace_AgentRunStream_ClosedMessageChannelDoesNotBusyLoop(t *testing.T) {
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	a.MCP = mcp.NewRegistry() // AgentRunStream calls WaitForInit unconditionally; NewForTest leaves it nil.

	entered := make(chan struct{})
	release := make(chan struct{})
	a.AgentCoordinator = &blockingStreamCoordinator{entered: entered, release: release}

	ch := make(chan pubsub.Event[message.Message])
	a.SetMessagesForTest(&controlledMessagesService{ch: ch})

	aw := NewAppWorkspace(a, configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir())))

	out, err := aw.AgentRunStream(t.Context(), "S1", "hello")
	require.NoError(t, err)
	go func() {
		for range out { //nolint:revive // drain so the producer never blocks
		}
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("run never entered Coordinator.Run")
	}

	// Simulate the broker closing the subscription mid-run, well before
	// Run (and therefore ctx) has any reason to end.
	close(ch)
	time.Sleep(20 * time.Millisecond) // let the fan-in goroutine notice

	var before, after syscall.Rusage
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &before))
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, syscall.Getrusage(syscall.RUSAGE_SELF, &after))
	cpu := timevalDiff(after.Utime, before.Utime) + timevalDiff(after.Stime, before.Stime)

	require.Less(t, cpu, 60*time.Millisecond,
		"a closed message-events channel must not busy-loop the fan-in goroutine")

	close(release)
}

func timevalDiff(a, b syscall.Timeval) time.Duration {
	return time.Duration(a.Nano() - b.Nano())
}
