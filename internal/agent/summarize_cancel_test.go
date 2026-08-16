package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSummarize_RegistersBeforeSessionRead(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var blockFirstRead sync.Once
	model := &raceInjectModel{text: "queued response"}
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)
	completionCtx, completionCancel := context.WithCancel(t.Context())
	defer completionCancel()
	completionCh := runCompleteBroker.Subscribe(completionCtx)
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		RunComplete:  runCompleteBroker,
		SystemPrompt: "system",
		Sessions: blockingSessionService{Service: env.sessions, get: func(ctx context.Context, id string) (session.Session, error) {
			block := false
			blockFirstRead.Do(func() { block = true })
			if !block {
				return env.sessions.Get(ctx, id)
			}
			close(readStarted)
			select {
			case <-releaseRead:
				return env.sessions.Get(ctx, id)
			case <-ctx.Done():
				return session.Session{}, ctx.Err()
			}
		}},
		Messages: env.messages,
	}).(*sessionAgent)

	summarizeErr := make(chan error, 1)
	go func() { summarizeErr <- sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil) }()
	<-readStarted
	require.True(t, sa.IsSessionBusy(sess.ID), "summarize must be active before session I/O")

	cancelDone := make(chan struct{})
	go func() {
		sa.Cancel(sess.ID)
		close(cancelDone)
	}()
	select {
	case <-cancelDone:
	case <-time.After(time.Second):
		t.Fatal("Cancel blocked on summarize session I/O")
	}

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "queued-run", Prompt: "queued"})
	require.NoError(t, err)
	require.Equal(t, 1, sa.QueuedPrompts(sess.ID), "Run must observe the active summarize entry")

	close(releaseRead)
	require.NoError(t, <-summarizeErr)
	select {
	case got := <-completionCh:
		require.Equal(t, "queued-run", got.Payload.RunID)
	case <-time.After(time.Second):
		t.Fatal("queued Run hung after summarize's empty-history exit")
	}
	require.False(t, sa.IsSessionBusy(sess.ID), "summarize and its queued Run must clean up")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "queued", msgs[0].Content().String())
	require.Equal(t, "queued response", msgs[1].Content().String())
}

func TestSummarize_CanceledParentHandsOffQueuedRuns(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	var blockFirstRead sync.Once
	model := &raceInjectModel{text: "response"}
	runCompleteBroker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(runCompleteBroker.Shutdown)
	completionCtx, completionCancel := context.WithCancel(t.Context())
	defer completionCancel()
	completionCh := runCompleteBroker.Subscribe(completionCtx)
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		RunComplete:  runCompleteBroker,
		SystemPrompt: "system",
		Sessions: blockingSessionService{Service: env.sessions, get: func(ctx context.Context, id string) (session.Session, error) {
			block := false
			blockFirstRead.Do(func() { block = true })
			if !block {
				return env.sessions.Get(ctx, id)
			}
			close(readStarted)
			<-releaseRead
			return session.Session{}, context.Canceled
		}},
		Messages: env.messages,
	}).(*sessionAgent)

	summarizeCtx, cancelSummarize := context.WithCancel(t.Context())
	summarizeErr := make(chan error, 1)
	go func() { summarizeErr <- sa.Summarize(summarizeCtx, sess.ID, fantasy.ProviderOptions{}, nil) }()
	<-readStarted

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "queued-first", Prompt: "first"})
	require.NoError(t, err)
	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "queued-second", Prompt: "second"})
	require.NoError(t, err)
	require.Equal(t, 2, sa.QueuedPrompts(sess.ID))

	cancelSummarize()
	close(releaseRead)
	require.ErrorIs(t, <-summarizeErr, context.Canceled)

	for _, runID := range []string{"queued-first", "queued-second"} {
		select {
		case got := <-completionCh:
			require.Equal(t, runID, got.Payload.RunID)
			require.False(t, got.Payload.Cancelled)
		case <-time.After(time.Second):
			t.Fatalf("queued Run %q did not publish RunComplete", runID)
		}
	}
	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the first queued Run must hand off to the next one")
	require.False(t, sa.IsSessionBusy(sess.ID))
}

func TestSummarize_CleansUpActiveEntryOnEarlyError(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	const sessionID = "missing"
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: &raceInjectModel{}, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "system",
		Sessions: blockingSessionService{Service: env.sessions, get: func(context.Context, string) (session.Session, error) {
			close(readStarted)
			<-releaseRead
			return session.Session{}, errors.New("session read failed")
		}},
		Messages: env.messages,
	}).(*sessionAgent)

	errCh := make(chan error, 1)
	go func() { errCh <- sa.Summarize(t.Context(), sessionID, fantasy.ProviderOptions{}, nil) }()
	<-readStarted

	newer := &activeCancel{cancel: func() {}}
	sa.dispatch.activeRequests.Set(sessionID, newer)
	close(releaseRead)
	require.ErrorContains(t, <-errCh, "failed to get session")

	got, ok := sa.dispatch.activeRequests.Get(sessionID)
	require.True(t, ok)
	require.Same(t, newer, got, "early cleanup must not erase a newer active entry")
}

type blockingSessionService struct {
	session.Service
	get func(context.Context, string) (session.Session, error)
}

func (s blockingSessionService) Get(ctx context.Context, id string) (session.Session, error) {
	return s.get(ctx, id)
}

// TestSummarize_QueuedThenCanceledDoesNotRun is the regression test for the
// bug this refactor closes: Summarize's post-completion handoff used to pop
// the next queued prompt with a raw messageQueue.Get/Set, without ever
// consulting the session's cancel mark - unlike Run's own end-of-turn
// handoff. A prompt queued while Summarize was still streaming and then
// canceled would therefore run anyway. drainNext is now the single
// implementation shared by both handoffs, so Summarize gets the same
// cancel-mark filter Run always had.
//
// model.onFinish fires while Summarize's activeRequests entry is still set
// (i.e. while the session is genuinely busy summarizing), the same timing
// TestRun_AutoSummarizeDoesNotClobberConcurrentActiveRequest uses to land a
// simulated concurrent event inside Summarize's own stream. It enqueues a
// follow-up directly on the dispatcher (mirroring how dispatch_cancel_test.go
// builds state by hand) and raises the cancel mark, without touching
// activeRequests itself - so Summarize's own stream completes normally and
// reaches its post-completion handoff, where the bug lived.
func TestSummarize_QueuedThenCanceledDoesNotRun(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Summarize requires at least one existing message to summarize.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	broker := pubsub.NewBroker[notify.RunComplete]()
	t.Cleanup(broker.Shutdown)
	subCtx, subCancel := context.WithCancel(t.Context())
	defer subCancel()
	ch := broker.Subscribe(subCtx)

	model := &raceInjectModel{text: "summary"}
	sa := NewSessionAgent(SessionAgentOptions{
		Model: Model{
			Model:      model,
			CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000},
		},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
		RunComplete:  broker,
	}).(*sessionAgent)

	// A follow-up is queued while summarization is still active, then a
	// cancel is recorded for the session. Neither touches activeRequests,
	// so Summarize's own stream is unaffected and completes normally.
	model.onFinish = func() {
		require.True(t, sa.IsSessionBusy(sess.ID), "onFinish must fire while Summarize is still active")
		sa.dispatch.enqueueCall(SessionAgentCall{SessionID: sess.ID, RunID: "run-followup", Prompt: "should-not-run"})
		sa.dispatch.cancelMark.Set(sess.ID, 1)
	}

	err = sa.Summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil)
	require.NoError(t, err)

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the canceled follow-up must not remain queued")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	for _, m := range msgs {
		require.NotEqual(t, "should-not-run", m.Content().String(),
			"the canceled follow-up must not have run")
	}

	// The dropped follow-up carried a RunID, so its terminal cancelled
	// RunComplete must still be published - otherwise a caller blocking on
	// that RunID (e.g. `braid run`) would hang.
	select {
	case got := <-ch:
		require.Equal(t, "run-followup", got.Payload.RunID)
		require.Equal(t, sess.ID, got.Payload.SessionID)
		require.True(t, got.Payload.Cancelled)
	case <-time.After(2 * time.Second):
		t.Fatal("expected a cancelled RunComplete for the dropped queued follow-up")
	}
}
