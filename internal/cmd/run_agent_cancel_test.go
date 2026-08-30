package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// fakeRunWorkspace is a minimal workspace.Workspace double for runAgent's
// loop. It embeds the (nil) interface so any method runAgent doesn't
// reach for in these tests panics loudly rather than silently doing
// nothing; only the surface runAgent actually calls is implemented below.
type fakeRunWorkspace struct {
	workspace.Workspace

	events chan workspace.AgentRunEvent
}

func (f *fakeRunWorkspace) InitCoderAgentNonInteractive(context.Context) error { return nil }

func (f *fakeRunWorkspace) Config() *config.Config {
	return &config.Config{Options: &config.Options{}}
}

func (f *fakeRunWorkspace) CreateSession(context.Context, string) (session.Session, error) {
	return session.Session{ID: "sess-1"}, nil
}

func (f *fakeRunWorkspace) AgentRunStream(context.Context, string, string) (<-chan workspace.AgentRunEvent, error) {
	return f.events, nil
}

// TestRunAgent_EventChannelClosesWhileCtxCancelled_ReturnsError is the
// regression test for run.go's consumer loop silently reporting success
// when AgentRunStream's channel closes without ever delivering a
// terminal (Done) event. Before the fix, the `if !ok { return nil }` arm
// returned nil unconditionally, so whenever the consumer's select landed
// on the events case instead of its own `case <-ctx.Done()` -- both
// ready at once, so Go's runtime picks between them at random -- a
// cancelled `sennit run` reported exit code 0, indistinguishable from a
// completed run to any script or CI job keying off it.
//
// Both ctx and the events channel are already done/closed *before*
// AgentRunStream is even called, so every iteration's first select has
// both cases ready simultaneously -- the exact race. It loops 50 times
// so a coin-flip bug fails reliably (about 1 in 2^50 to pass by chance),
// while the fix passes every time because both cases now agree: whichever
// one Go's select happens to pick, it returns ctx.Err().
func TestRunAgent_EventChannelClosesWhileCtxCancelled_ReturnsError(t *testing.T) {
	const iterations = 50
	for i := 0; i < iterations; i++ {
		events := make(chan workspace.AgentRunEvent)
		close(events)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		ws := &fakeRunWorkspace{events: events}

		resultCh := make(chan error, 1)
		go func() {
			resultCh <- runAgent(ctx, ws, "hello", "", true, "", false)
		}()

		select {
		case err := <-resultCh:
			require.Error(t, err, "iteration %d: a channel close under a cancelled ctx must not be reported as success", i)
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: runAgent never returned", i)
		}
	}
}

// TestRunAgent_CleanFinish_ReturnsNil is the companion case: a normal
// terminal event (Done: true, Err: nil) must still report success, so
// the fix above does not turn every run into a false failure.
func TestRunAgent_CleanFinish_ReturnsNil(t *testing.T) {
	events := make(chan workspace.AgentRunEvent, 1)
	events <- workspace.AgentRunEvent{Done: true}
	close(events)
	ws := &fakeRunWorkspace{events: events}

	err := runAgent(context.Background(), ws, "hello", "", true, "", false)
	require.NoError(t, err)
}
