package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// coordinatorDeliverOnly is a minimal CompletionDeliverer for tests that
// only exercise the SendToParent delivery path: it forwards into a real
// sessionAgent, so a message rides the exact same delivery path (enqueue,
// at-most-once, idle-wake) DeliverTaskCompletion's own tests already
// exercise. Nothing else needs stubbing - a DelegationParent holds only
// the delivery slice of a coordinator.
type coordinatorDeliverOnly struct {
	sa *sessionAgent
}

func (c *coordinatorDeliverOnly) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	c.sa.DeliverTaskCompletion(ctx, sessionID, completion)
}

// registerSelfParent registers childSessionID's parent as parentSessionID
// on sa itself, mirroring the "task shares its parent's own Coordinator"
// case DelegationParent's doc comment describes.
func registerSelfParent(sa *sessionAgent, childSessionID, parentSessionID string) {
	sa.RegisterDelegationParent(childSessionID, DelegationParent{
		Parent:          &coordinatorDeliverOnly{sa: sa},
		ParentSessionID: parentSessionID,
		DelegationID:    "task-1",
		Kind:            "task",
		Name:            "task-name",
		Depth:           0,
	})
}

// promptContains reports whether any text part of any message in prompt
// contains substr.
func promptContains(prompt fantasy.Prompt, substr string) bool {
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, substr) {
				return true
			}
		}
	}
	return false
}

// TestSendToParent_WakesIdleParentAndDeliversOnce proves the core
// SendToParent path end to end: a mid-run ask reaches the parent
// session's next step via an idle-wake continuation (mirroring
// TestDeliverTaskCompletion_WakesIdleSessionExactlyOnce), attributed with
// the delegation's id/kind/name, and exactly once.
func TestSendToParent_WakesIdleParentAndDeliversOnce(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	parentSess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	// Give the parent prior history ending in an assistant message, same
	// shape TestDeliverTaskCompletion_WakesIdleSessionExactlyOnce relies
	// on to prove the wake path supplies its own placeholder prompt.
	_, err = env.messages.Create(t.Context(), parentSess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	const childSessionID = "child-session-ask"
	registerSelfParent(sa, childSessionID, parentSess.ID)

	err = sa.SendToParent(t.Context(), childSessionID, "ask-marker-text")
	require.NoError(t, err)

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"the continuation turn must actually reach the model")

	require.Empty(t, sa.drainCompletionsForStep(parentSess.ID),
		"the inbox must be empty: the message was consumed by the wake, not left behind")

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one turn must have reached the model")
	require.True(t, promptContains(prompts[0], "ask-marker-text"), "the model must see the ask text")
	require.True(t, promptContains(prompts[0], "task-1"), "the model must see the delegation id")
	require.True(t, promptContains(prompts[0], "task-name"), "the model must see the delegation name")
	require.Equal(t, 1, occurrencesAcross(prompts, "ask-marker-text"), "the ask must reach the model exactly once")
}

// occurrencesAcross counts how many text parts across prompts contain
// substr, mirroring promptRecordingModel.occurrences but over an explicit
// slice rather than a live model.
func occurrencesAcross(prompts []fantasy.Prompt, substr string) int {
	var n int
	for _, prompt := range prompts {
		for _, msg := range prompt {
			for _, part := range msg.Content {
				if text, ok := part.(fantasy.TextPart); ok && strings.Contains(text.Text, substr) {
					n++
				}
			}
		}
	}
	return n
}

// TestSendToParent_BusyParentDoesNotStartSecondTurn mirrors
// TestDeliverTaskCompletion_DuringActiveTurnDoesNotStartSecond: a message
// arriving while the parent session is busy must not start a second turn.
// It stays queued in the inbox for the active turn's own next step.
func TestSendToParent_BusyParentDoesNotStartSecondTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &cancelAwareGatedModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	parentSess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: parentSess.ID, Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}
	require.True(t, sa.IsSessionBusy(parentSess.ID))

	const childSessionID = "child-session-busy"
	registerSelfParent(sa, childSessionID, parentSess.ID)

	err = sa.SendToParent(t.Context(), childSessionID, "busy-ask-marker")
	require.NoError(t, err)

	// Still exactly the one active turn - no second Run was started.
	require.True(t, sa.IsSessionBusy(parentSess.ID))
	queued := sa.drainCompletionsForStep(parentSess.ID)
	require.Len(t, queued, 1)
	require.True(t, queued[0].IsMessage)
	require.Equal(t, "busy-ask-marker", queued[0].Message)

	close(model.gate)
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never finished")
	}
}

// TestSendToParent_AndCompletionBothSurviveSameDrain proves a message and
// a completion landing "at the same time" (enqueued back to back before
// the parent's next drain) both survive and are both folded in - neither
// lost nor duplicated.
//
// The parent is held mid-stream on a gate for the same reason
// TestSendToParent_BusyParentDoesNotStartSecondTurn holds it: SendToParent
// rides DeliverTaskCompletion, whose idle-wake would otherwise start a
// continuation that drains the inbox from its own goroutine and races the
// drain below - the drain the test is here to make an assertion about. It
// lost that race under -race on CI and found an empty queue.
func TestSendToParent_AndCompletionBothSurviveSameDrain(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &cancelAwareGatedModel{
		text:    "done",
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)

	parentSess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)

	runDone := make(chan error, 1)
	go func() {
		_, runErr := sa.Run(t.Context(), SessionAgentCall{SessionID: parentSess.ID, Prompt: "main"})
		runDone <- runErr
	}()

	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("main run never entered Stream")
	}
	require.True(t, sa.IsSessionBusy(parentSess.ID))

	const childSessionID = "child-session-both"
	registerSelfParent(sa, childSessionID, parentSess.ID)

	// The low-level dispatcher primitive for the completion, mirroring
	// TestPrepareStep_CompletionRequeuedOnStepFailure: this isolates the
	// enqueue itself rather than also exercising DeliverTaskCompletion's
	// own idle-wake attempt, which SendToParent below already covers.
	completion := testCompletion("both-completion-marker")
	sa.enqueueCompletion(parentSess.ID, completion)

	err = sa.SendToParent(t.Context(), childSessionID, "both-message-marker")
	require.NoError(t, err)

	drained := sa.drainCompletionsForStep(parentSess.ID)
	require.Len(t, drained, 2, "both the completion and the message must survive in the same drain")

	var sawCompletion, sawMessage bool
	for _, c := range drained {
		if !c.IsMessage && c.ResultText == "both-completion-marker" {
			sawCompletion = true
		}
		if c.IsMessage && c.Message == "both-message-marker" {
			sawMessage = true
		}
	}
	require.True(t, sawCompletion, "the completion must not be lost")
	require.True(t, sawMessage, "the message must not be lost")

	require.Empty(t, sa.drainCompletionsForStep(parentSess.ID), "a second drain must find nothing left - neither event should be duplicated")

	close(model.gate)
	select {
	case runErr := <-runDone:
		require.NoError(t, runErr)
	case <-time.After(5 * time.Second):
		t.Fatal("run never finished")
	}
}

// TestSendToParent_NoRegisteredParentReturnsError proves the parentless
// case: no registration for sessionID must return a clear, non-nil error
// and must not deliver anything or panic.
func TestSendToParent_NoRegisteredParentReturnsError(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &promptRecordingModel{text: "done"}, "system").(*sessionAgent)

	err := sa.SendToParent(t.Context(), "no-such-session", "orphan-message")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no-such-session")
}

// TestSendToParent_PersonsSessionIsWokenToo is the ask-side half of the
// wake path: a delegation speaking up mid-run reaches the person who
// started it right away, rather than sitting unread until they happen to
// type. It is the same continuation the completion side uses.
func TestSendToParent_PersonsSessionIsWokenToo(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	model := &promptRecordingModel{text: "done"}
	sa := testSessionAgent(env, model, "system").(*sessionAgent)

	parentSess, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), parentSess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "earlier answer"}},
	})
	require.NoError(t, err)

	// The child is a delegation; its parent is not - it is the person's
	// own session, so no registerSelfParent call for it.
	const childSessionID = "child-session-ask-person"
	registerSelfParent(sa, childSessionID, parentSess.ID)

	require.NoError(t, sa.SendToParent(t.Context(), childSessionID, "ask-reaches-the-person"))

	require.Eventually(t, func() bool { return model.count() > 0 }, 2*time.Second, 5*time.Millisecond,
		"an ask must wake the session that started the delegation")

	prompts := model.snapshotPrompts()
	require.Len(t, prompts, 1, "exactly one continuation turn must have reached the model")
	require.True(t, promptContains(prompts[0], "ask-reaches-the-person"),
		"the woken turn must carry the ask")
	require.Empty(t, sa.drainCompletionsForStep(parentSess.ID), "that turn must have drained the inbox")
}
