package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// raceInjectModel is a minimal fantasy.LanguageModel that streams a single
// text part followed by a normal finish, invoking onFinish (if set) right
// before the terminal Finish part is yielded. Tests use onFinish to land a
// simulated concurrent dispatch precisely at the point a real one would have
// to win the session: after this run's own Stream call has produced its
// step, but before the caller has processed shouldSummarize.
type raceInjectModel struct {
	text     string
	onFinish func()
}

func (m *raceInjectModel) Provider() string { return "fake" }
func (m *raceInjectModel) Model() string    { return "fake-model" }

func (m *raceInjectModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
	}, nil
}

func (m *raceInjectModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	text := m.text
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		if m.onFinish != nil {
			m.onFinish()
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *raceInjectModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *raceInjectModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

// TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest covers the
// review finding (2.3) that the auto-summarize branch released its
// activeRequests entry with an unconditional Del, unlike every other
// cleanup path in Run/Summarize, which use CompareAndDelete against the
// run's own *activeCancel. An unconditional Del there can erase a
// different, newer run's entry that won the session in the busy window
// this release opens (Run stops looking busy for the moment between this
// release and Summarize's own busy check / registration).
//
// A context window of 1 forces the StopWhen auto-summarize condition to
// fire after the first step regardless of actual token usage. onFinish
// fires after the model has produced its step but before Run processes
// shouldSummarize, installing a second *activeCancel directly - exactly
// what a real concurrent dispatch's Run would do via activeRequests.Set
// had it raced into the same window. The real race window (between the
// Del/CompareAndDelete call and Summarize's own busy check) is a few
// non-blocking statements wide with no test seam to hook without adding
// production code solely for testing, so this drives the same map
// mutation through the real Run call instead of trying to land two actual
// goroutines in that window.
type continuationRaceModel struct {
	streams        atomic.Int32
	summaryStarted chan struct{}
	releaseSummary chan struct{}
}

func (m *continuationRaceModel) Provider() string { return "fake" }
func (m *continuationRaceModel) Model() string    { return "fake-model" }

func (m *continuationRaceModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{FinishReason: fantasy.FinishReasonStop}, nil
}

func (m *continuationRaceModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	stream := m.streams.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		switch stream {
		case 1:
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "tool", ToolCallName: "hold"}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: "tool", Delta: `{}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: "tool"}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "tool", ToolCallName: "hold", ToolCallInput: `{}`})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		case 2:
			close(m.summaryStarted)
			<-m.releaseSummary
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		default:
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "concurrent"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}
	}, nil
}

func (m *continuationRaceModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *continuationRaceModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not implemented")
}

type blockingNotificationPublisher struct {
	published chan struct{}
	release   chan struct{}
	blocked   atomic.Bool
}

// Publish blocks only on the first call - the parked run's own
// TypeAgentFinished notification, which these tests use to land a
// simulated concurrent dispatch precisely at that pause point. Now that
// queue mutations (e.g. ClearQueue) also publish their own
// notify.TypeQueueChanged notification (see sessionAgent.
// publishQueueChanged), a second call can legitimately arrive while the
// first is still parked here; sync.Once would serialize that second
// call behind the first's in-flight block, deadlocking the two, so this
// uses a CompareAndSwap instead: only the call that wins it blocks,
// every other caller returns immediately.
func (p *blockingNotificationPublisher) Publish(pubsub.EventType, notify.Notification) {
	if p.blocked.CompareAndSwap(false, true) {
		close(p.published)
		<-p.release
	}
}

func (*blockingNotificationPublisher) PublishMustDeliver(context.Context, pubsub.EventType, notify.Notification) {
}

func TestRun_AutoSummarizeContinuationClearQueueCompletesOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &continuationRaceModel{summaryStarted: make(chan struct{}), releaseSummary: make(chan struct{})}
	notifications := &blockingNotificationPublisher{published: make(chan struct{}), release: make(chan struct{})}
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
		Notify:   notifications,
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	completion := make(chan notify.RunComplete, 2)
	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "original",
			Prompt:    "original",
			OnComplete: func(event notify.RunComplete) {
				completion <- event
			},
		})
		runDone <- runErr
	}()

	<-model.summaryStarted
	close(model.releaseSummary)
	<-notifications.published
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID))

	sa.ClearQueue(sess.ID)
	close(notifications.release)
	require.NoError(t, <-runDone)

	event := <-completion
	require.Equal(t, "original", event.RunID)
	require.True(t, event.Cancelled)
	select {
	case duplicate := <-completion:
		t.Fatalf("expected exactly one RunComplete for original RunID, got duplicate: %+v", duplicate)
	default:
	}
}

// TestRun_AutoSummarizeContinuationPreservesAcceptedSequence is the
// regression test for a Cancel that lands on an unrelated accepted sibling
// (cancelAnchor here) while "original" is busy summarizing: it must not
// poison "original"'s own post-summary continuation. requeueContinuation
// used to carry the continuation's acceptSeq forward as 0 ("untracked"),
// which canceledBySeq treats as covered by *any* pending cancel mark - so
// the continuation was silently dropped despite having nothing to do with
// cancelAnchor. The fix stamps a fresh accept sequence when the
// continuation is requeued, which the earlier cancel's mark cannot cover
// since it was recorded before this call ever entered the queue.
func TestRun_AutoSummarizeContinuationPreservesAcceptedSequence(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &continuationRaceModel{summaryStarted: make(chan struct{}), releaseSummary: make(chan struct{})}
	hold := fantasy.NewAgentTool("hold", "hold", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Tools:    []fantasy.AgentTool{hold},
	}).(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	original := sa.BeginAccepted(sess.ID)
	completion := make(chan notify.RunComplete, 4)
	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{
			SessionID: sess.ID,
			RunID:     "original",
			Prompt:    "original",
			Accepted:  original,
			OnComplete: func(event notify.RunComplete) {
				completion <- event
			},
		})
		runDone <- runErr
	}()
	<-model.summaryStarted

	cancelAnchor := sa.BeginAccepted(sess.ID)
	active := &activeCancel{cancel: func() {}}
	sa.setActiveForTest(sess.ID, active)
	sa.cancel(sess.ID)
	concurrent := sa.BeginAccepted(sess.ID)
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "concurrent", Accepted: concurrent})
	require.NoError(t, err)
	sa.clearActiveIfMatch(sess.ID, active)

	close(model.releaseSummary)
	require.NoError(t, <-runDone)
	cancelAnchor.Close()

	// [1] original's own turn, [2] its summarize pass, [3] "concurrent"'s
	// turn, [4] "concurrent"'s own auto-summarize, [5] original's
	// continuation resuming, [6] the continuation's own auto-summarize.
	// A regression that let cancelAnchor's cancel poison the continuation
	// again would stop at 4: the continuation would never get its own
	// turn.
	require.Equal(t, int32(6), model.streams.Load(), "the post-cancel enqueue must run and the continuation must still resume")
	event := <-completion
	require.Equal(t, "original", event.RunID)
	require.False(t, event.Cancelled, "the continuation completed on its own, unrelated to cancelAnchor's cancel")
	select {
	case duplicate := <-completion:
		t.Fatalf("expected exactly one RunComplete for original RunID, got duplicate: %+v", duplicate)
	default:
	}
}

func TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	var racerCanceled atomic.Bool
	racer := &activeCancel{cancel: func() { racerCanceled.Store(true) }}

	model := &raceInjectModel{text: "done"}
	sa := NewSessionAgent(SessionAgentOptions{
		Model: Model{
			Model:      model,
			CatalogCfg: catwalk.Model{ContextWindow: 1, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	model.onFinish = func() {
		sa.setActiveForTest(sess.ID, racer)
	}

	_, err = sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "hello",
	})
	// Summarize's own busy check observes the racer's entry and bails with
	// ErrSessionBusy - the visible symptom that the racer's entry survived
	// the shouldSummarize cleanup instead of being erased. That failure is
	// no longer fatal to the turn's own completion bookkeeping (finishTurn
	// logs it and still tears down normally, see the summarize-failure
	// fix), so the turn itself reports success here; ErrSessionBusy is
	// only ever visible as the logged error and this turn's RunComplete,
	// not as Run's returned error.
	require.NoError(t, err)

	got, ok := sa.getActiveForTest(sess.ID)
	require.True(t, ok, "the concurrently-installed entry must survive this run's cleanup")
	require.Same(t, racer, got, "this run's cleanup must not replace a newer run's entry")
	require.False(t, racerCanceled.Load(), "this run's cleanup must not cancel a newer run's context")
}
