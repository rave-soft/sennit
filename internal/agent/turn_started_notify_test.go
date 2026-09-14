package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/stretchr/testify/require"
)

// TestTurnStarted_PublishedOncePerTurn pins the event a UI turn clock
// needs: one notify.TypeTurnStarted per turn that genuinely became the
// session's active run, carrying that run's ID.
//
// The clock used to be started by the client that sent the prompt, which
// covered only the turns a client asked for. Nothing announced a turn,
// so a turn the session started for itself ran with no elapsed time at
// all.
func TestTurnStarted_PublishedOncePerTurn(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	notifier := &recordingNotifier{}
	sa := NewSessionAgent(SessionAgentOptions{
		Model:    Model{Model: &finishStreamModel{text: "done"}, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		Sessions: env.sessions,
		Messages: env.messages,
		Notify:   notifier,
	}).(*sessionAgent)

	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "run-1", Prompt: "first"})
	require.NoError(t, err)
	require.Equal(t, 1, notifier.count(sess.ID, notify.TypeTurnStarted),
		"a turn that became the active run must announce itself exactly once")

	_, err = sa.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, RunID: "run-2", Prompt: "second"})
	require.NoError(t, err)

	started := notifier.ofType(notify.TypeTurnStarted)
	require.Len(t, started, 2, "each turn announces itself; the count is per turn, not per session")
	require.Equal(t, []string{"run-1", "run-2"}, []string{started[0].RunID, started[1].RunID},
		"the announcement carries its own run's ID so an observer can attribute it")
}

// TestTurnStarted_NotPublishedWhenTheCallIsOnlyQueued is the control: a
// prompt that lands behind a busy session has not started a turn, and
// announcing one there would start a clock for work that is not running
// yet — and reset the clock of the turn that is.
func TestTurnStarted_NotPublishedWhenTheCallIsOnlyQueued(t *testing.T) {
	t.Parallel()
	env := testEnv(t)

	notifier := &recordingNotifier{}
	sa := NewSessionAgent(SessionAgentOptions{
		Sessions: env.sessions,
		Messages: env.messages,
		Notify:   notifier,
	}).(*sessionAgent)

	const sessionID = "busy-session"
	sa.setActiveForTest(sessionID, &activeCancel{cancel: func() {}})

	_, err := sa.Run(t.Context(), SessionAgentCall{SessionID: sessionID, Prompt: "follow-up"})
	require.NoError(t, err)

	require.Equal(t, 1, sa.QueuedPrompts(sessionID), "the follow-up must actually be queued")
	require.Zero(t, notifier.count(sessionID, notify.TypeTurnStarted),
		"a queued prompt has not started a turn")
}
