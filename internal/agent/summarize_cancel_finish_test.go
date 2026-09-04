package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// summarizeCancelModel plays the two requests finishTurn's auto-summarize
// tail makes: the turn's own step (an ordinary text finish - a context
// window of 1 trips the auto-summarize StopWhen condition regardless of
// finish reason, see TestStopOnContextWindow_UnknownHistorySizeStillSummarizes),
// then the summarize pass, which blocks until the test cancels the
// session and reports the cancellation the way a real model does when a
// canceled context finally surfaces mid-stream.
type summarizeCancelModel struct {
	calls       atomic.Int32
	entered     chan struct{}
	enteredOnce sync.Once
	release     chan struct{}
}

func (m *summarizeCancelModel) Provider() string { return "fake" }
func (m *summarizeCancelModel) Model() string    { return "fake-model" }

func (m *summarizeCancelModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	n := m.calls.Add(1)
	if n == 1 {
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "hi"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		// Once: a queued follow-up runs a second turn through this same
		// model, and closing an already-closed channel would panic before
		// the test could assert anything.
		m.enteredOnce.Do(func() { close(m.entered) })
		<-m.release
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: context.Canceled})
	}, nil
}

func (m *summarizeCancelModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *summarizeCancelModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (m *summarizeCancelModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

// TestRunTurn_CancelDuringAutoSummarizeReportsCancelled is the regression
// test for G4: the "nothing queued behind this turn" branch of
// completeTurn used to publish a RunComplete with only Error set - not
// Cancelled - when the summarize pass finishTurn ran was itself canceled
// (summarize's context derives from the turn's own genCtx, so an Escape
// mid-summarize surfaces here as summarizeFailed wrapping
// context.Canceled). thread/lifecycle.go reads a non-empty Error as
// StatusFailed, so a cancel during auto-summarize was reported to the
// user as "failed: context canceled" instead of a plain cancellation, and
// TypeAgentFinished ("Task finished") was published right alongside it.
func TestRunTurn_CancelDuringAutoSummarizeReportsCancelled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	notifyBroker := pubsub.NewBroker[notify.Notification]()
	t.Cleanup(notifyBroker.Shutdown)
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)

	model := &summarizeCancelModel{entered: make(chan struct{}), release: make(chan struct{})}
	sa := NewSessionAgent(SessionAgentOptions{
		// A window of 1 puts every turn over the auto-summarize threshold
		// on its very first step.
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions:    env.sessions,
		Messages:    env.messages,
		Notify:      notifyBroker,
		RunComplete: runCompleteBroker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer subCancel()
	completions := runCompleteBroker.Subscribe(subCtx)
	notifications := notifyBroker.Subscribe(subCtx)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "main", Prompt: "hello"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the summarize pass never reached its blocking Stream call")
	}

	sa.Cancel(sess.ID)
	close(model.release)

	select {
	case runErr := <-runDone:
		_ = runErr
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}

	select {
	case evt := <-completions:
		require.Equal(t, "main", evt.Payload.RunID)
		require.True(t, evt.Payload.Cancelled, "a cancel mid-summarize must report Cancelled, not a bare Error")
		require.NotEmpty(t, evt.Payload.Error)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the main turn's RunComplete")
	}

	// No TypeAgentFinished ("Task finished") for a turn a cancel actually
	// interrupted - AgentDispatcher.run's own TypeAgentError path (fired
	// on a non-nil, non-cancel error) owns turns that genuinely failed,
	// and a canceled summarize is neither a finish nor a failure worth
	// announcing twice.
	select {
	case n := <-notifications:
		t.Fatalf("unexpected notification published: %+v", n.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRunTurn_CancelDuringAutoSummarizeWithQueuedPromptReportsCancelled is
// the other half of the test above, and it exists because the first fix
// for this closed only one of the two branches that publish the turn's
// RunComplete. Which branch runs is decided by nothing more than whether
// a prompt was waiting in the queue when the turn ended - the user typing
// their next message right after pressing Escape is enough - and the
// queued branch reported the cancel as a plain error, so a thread
// finalized as "failed: context canceled" exactly as before.
func TestRunTurn_CancelDuringAutoSummarizeWithQueuedPromptReportsCancelled(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)

	model := &summarizeCancelModel{entered: make(chan struct{}), release: make(chan struct{})}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:       Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions:    env.sessions,
		Messages:    env.messages,
		RunComplete: runCompleteBroker,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	subCtx, subCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer subCancel()
	completions := runCompleteBroker.Subscribe(subCtx)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "main", Prompt: "hello"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the summarize pass never reached its blocking Stream call")
	}

	// Cancel first, then queue: a cancel clears whatever is already
	// queued, so the prompt has to arrive after it - which is exactly the
	// real sequence, someone pressing Escape and immediately typing their
	// next message. The turn then ends with something to drain and takes
	// the outerOwesRunComplete branch.
	sa.Cancel(sess.ID)
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, RunID: "queued", Prompt: "next"})
	close(model.release)

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never returned")
	}

	for {
		select {
		case evt := <-completions:
			if evt.Payload.RunID != "main" {
				continue
			}
			require.True(t, evt.Payload.Cancelled,
				"a cancel mid-summarize must report Cancelled even when a prompt was queued behind the turn")
			require.NotEmpty(t, evt.Payload.Error)
			return
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the main turn's RunComplete")
		}
	}
}
