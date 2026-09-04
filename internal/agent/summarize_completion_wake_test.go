package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// blockingSummaryStreamModel blocks the summary request's own Stream call
// until released, so a test can land a completion while a summarize
// genuinely still holds the session's active slot - the exact window
// wakeEligibleLocked's `s.active == nil` check exists to detect.
type blockingSummaryStreamModel struct {
	started chan struct{}
	release chan struct{}
}

func (m *blockingSummaryStreamModel) Provider() string { return "fake" }
func (m *blockingSummaryStreamModel) Model() string    { return "fake-model" }

func (m *blockingSummaryStreamModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if isTitleCall(call) {
		return titleStream()
	}
	return func(yield func(fantasy.StreamPart) bool) {
		close(m.started)
		<-m.release
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: "summary"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func (m *blockingSummaryStreamModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, errors.New("not used")
}

func (m *blockingSummaryStreamModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("not used")
}

func (m *blockingSummaryStreamModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("not used")
}

// TestSummarize_WakesFromInboxAfterReleasingTheActiveSlot is the
// regression test for DEFECT 1: a completion delivered while a summarize
// holds the active slot used to have no way to wake the parked delegation
// session it belongs to - summarize's own deferred teardown never called
// wakeFromInboxIfIdle, unlike runTurn's (see usage.go). The parent session
// would sit at StatusRunning until the idle watchdog eventually noticed,
// rather than resuming the moment the summary actually finished.
func TestSummarize_WakesFromInboxAfterReleasingTheActiveSlot(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	// A summarize bails immediately on an empty session, never reaching
	// its own Stream call; seed one message so this summarize actually
	// blocks in the model.
	_, err = env.messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	model := &blockingSummaryStreamModel{started: make(chan struct{}), release: make(chan struct{})}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: model, CatalogCfg: catwalkModel()},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	// A parked delegation session is exactly what the idle sweep
	// summarizes in practice - see markActivity's own comment on why the
	// sweep reaches these sessions at all - so that is the shape this
	// test uses, though the wake path itself no longer asks what kind of
	// session it is waking.
	sa.SetLiveSession(sess.ID)
	sa.RegisterDelegationParent(sess.ID, DelegationParent{
		ParentSessionID: "parent-of-" + sess.ID,
		DelegationID:    "delegation-1",
		Kind:            "task",
	})

	woke := make(chan string, 1)
	sa.continuationRunner = func(_ context.Context, sessionID string) error {
		woke <- sessionID
		return nil
	}

	summarizeDone := make(chan error, 1)
	go func() {
		summarizeDone <- sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, nil, sa.model.Get(), "", nil, nil)
	}()

	select {
	case <-model.started:
	case <-time.After(5 * time.Second):
		t.Fatal("summarize never reached its Stream call")
	}
	require.True(t, sa.IsSessionBusy(sess.ID), "summarize must hold the active slot while it streams")

	sa.DeliverTaskCompletion(t.Context(), sess.ID, TaskCompletion{
		DelegationID: "delegation-1",
		Kind:         "task",
		Status:       "completed",
		ResultText:   "child finished",
	})

	// The completion landed while summarize still owns the active slot:
	// wakeEligibleLocked requires s.active == nil, so nothing wakes yet.
	select {
	case <-woke:
		t.Fatal("nothing should wake while the summarize still holds the active slot")
	case <-time.After(100 * time.Millisecond):
	}

	close(model.release)

	select {
	case err := <-summarizeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("summarize never finished")
	}

	select {
	case woken := <-woke:
		require.Equal(t, sess.ID, woken, "the parked session must be woken once summarize releases the active slot")
	case <-time.After(5 * time.Second):
		t.Fatal("wakeFromInboxIfIdle never woke the parked session after summarize finished - it would otherwise sit at StatusRunning until the watchdog")
	}
	require.False(t, sa.IsSessionBusy(sess.ID), "summarize must release the slot once it's done")
}
