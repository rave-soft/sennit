package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/stretchr/testify/require"
)

// recordingNotifier collects every published notify.Notification so a
// test can assert on which events fired, without standing up a real
// pubsub broker and subscriber.
type recordingNotifier struct {
	mu  sync.Mutex
	got []notify.Notification
}

func (n *recordingNotifier) Publish(_ pubsub.EventType, notification notify.Notification) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got = append(n.got, notification)
}

func (*recordingNotifier) PublishMustDeliver(context.Context, pubsub.EventType, notify.Notification) {
}

// count returns how many recorded notifications match typ and sessionID.
func (n *recordingNotifier) count(sessionID string, typ notify.Type) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	var c int
	for _, got := range n.got {
		if got.Type == typ && got.SessionID == sessionID {
			c++
		}
	}
	return c
}

// TestQueueChanged_PublishedOnEnqueue covers the enqueue entry point
// (run's busy branch, agent.go): queuing a follow-up behind an active
// turn must publish a TypeQueueChanged notification carrying the
// session's ID, so the UI's queued-prompt pill does not have to wait out
// the TTL backstop to notice.
func TestQueueChanged_PublishedOnEnqueue(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	notifier := &recordingNotifier{}
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
		Notify:   notifier,
	}).(*sessionAgent)

	const sessionID = "busy-session"
	// Mark the session busy so Run takes the enqueue branch without
	// needing a real streaming model.
	sa.dispatch.activeRequests.Set(sessionID, &activeCancel{cancel: func() {}})

	_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "follow-up"})
	require.NoError(t, err)

	require.Equal(t, 1, sa.QueuedPrompts(sessionID), "the follow-up must actually be queued")
	require.Equal(t, 1, notifier.count(sessionID, notify.TypeQueueChanged),
		"enqueueing behind a busy session must publish exactly one queue-changed notification")
}

// TestQueueChanged_PublishedOnDrain covers the drain entry point
// (drainQueueForStep, dispatch.go): a follow-up queued before a turn
// starts gets folded into the turn's first PrepareStep call, which must
// publish a TypeQueueChanged notification.
func TestQueueChanged_PublishedOnDrain(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	notifier := &recordingNotifier{}
	model := &finishStreamModel{text: "done"}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: model, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Notify:   notifier,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	// Queue directly (bypassing Run's busy branch, and so its own
	// enqueue notification) so this test isolates the drain path.
	sa.enqueueCall(SessionAgentCall{SessionID: sess.ID, Prompt: "queued-followup"})
	require.Zero(t, notifier.count(sess.ID, notify.TypeQueueChanged),
		"a direct enqueueCall bypasses run's busy branch and must not itself publish")

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "main"})
	require.NoError(t, err)

	require.Equal(t, 0, sa.QueuedPrompts(sess.ID), "the queued follow-up must have been drained")
	require.Equal(t, 1, notifier.count(sess.ID, notify.TypeQueueChanged),
		"draining a queued follow-up into the turn's first step must publish exactly one queue-changed notification")
}

// lockReentryNotifier's Publish tries (non-blocking) to take sessionID's
// own dispatch mutex, on every call. This is a structural check, not a
// timing one: if notifyQueueChanged ever ran while that mutex was still
// held, this TryLock would observe it locked (by this very goroutine,
// since sync.Mutex is not reentrant) and return false - deterministically,
// with no window to race or wait out - and it never blocks, so it cannot
// itself deadlock the caller even when the rule is violated. violations
// (not just "did the last call succeed") is what's asserted on: a
// well-behaved later call must not be able to paper over an earlier one
// that fired too early.
type lockReentryNotifier struct {
	dispatch   *dispatcher
	sessionID  string
	calls      atomic.Int32
	violations atomic.Int32
}

func (n *lockReentryNotifier) Publish(pubsub.EventType, notify.Notification) {
	n.calls.Add(1)
	mu := n.dispatch.sessionMu(n.sessionID)
	if mu.TryLock() {
		mu.Unlock()
	} else {
		n.violations.Add(1)
	}
}

func (*lockReentryNotifier) PublishMustDeliver(context.Context, pubsub.EventType, notify.Notification) {
}

// TestQueueChanged_NotPublishedUnderDispatchLock proves the "publish
// after releasing the lock" rule structurally for dispatcher.cancel: its
// notifyQueueChanged call is deferred to run after mu.Unlock (defers run
// LIFO - see cancel's comments), so a Publish callback that tries to
// take the same session's dispatch mutex must always succeed.
func TestQueueChanged_NotPublishedUnderDispatchLock(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	const sessionID = "session"
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
	}).(*sessionAgent)
	notifier := &lockReentryNotifier{dispatch: sa.dispatch, sessionID: sessionID}
	sa.notify = notifier

	// Seed a non-empty queue directly so cancel's tail actually clears
	// something (and so takes the branch that calls notifyQueueChanged).
	sa.dispatch.messageQueue.Set(sessionID, []SessionAgentCall{{SessionID: sessionID, Prompt: "queued"}})

	sa.Cancel(sessionID)

	require.Positive(t, notifier.calls.Load(), "cancel must have published a queue-changed notification at all")
	require.Zero(t, notifier.violations.Load(),
		"notifyQueueChanged must only run after cancel releases the dispatch mutex")
}
