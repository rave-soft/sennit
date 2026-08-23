package agent

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestSummarize_ClaimHandsOffTheActiveSlotAtomically is the regression test
// for the idle window between finishTurn's clearActiveIfMatch and
// summarize's own busy check: finishTurn used to release its active-run
// slot and let summarize re-claim it from scratch, as two separately
// locked steps. A queued continuation could claim the session in that
// window, so a perfectly successful turn's summarize call would fail with
// ErrSessionBusy even though nothing was actually wrong. finishTurn now
// hands its still-installed *activeCancel to summarize via the claim
// parameter, which swaps the slot atomically instead of releasing and
// re-claiming.
//
//nolint:tparallel // the subtests share one sessionAgent and session; they must run in sequence.
func TestSummarize_ClaimHandsOffTheActiveSlotAtomically(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sess, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)

	sa := NewSessionAgent(SessionAgentOptions{
		Model:        Model{Model: &raceInjectModel{text: "summary"}, CatalogCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt: "system",
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)

	t.Run("passing claim takes over the caller's own still-installed slot", func(t *testing.T) {
		// This is what finishTurn does now: the slot is never released
		// before summarize gets a chance to take it over, so there is no
		// window for anything else to claim the session in between.
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, sa.model.Get(), "", nil, ac)
		require.NoError(t, err, "summarize must succeed by swapping in its own slot, not by finding the session idle")
		require.False(t, sa.IsSessionBusy(sess.ID), "summarize must release the slot once it's done")
	})

	t.Run("the old release-then-reclaim sequence loses the slot to a racer", func(t *testing.T) {
		// Reproduces the bug directly: release the slot the way finishTurn
		// used to (clearActiveIfMatch), let a racer (standing in for a
		// queued continuation) claim the now-idle session, then call
		// summarize the old way (claim=nil, re-claiming from scratch). It
		// observes the racer and bails with ErrSessionBusy - the exact
		// symptom a successful turn's finishTurn used to hit.
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		sa.clearActiveIfMatch(sess.ID, ac)
		racer := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, racer)

		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, sa.model.Get(), "", nil, nil)
		require.ErrorIs(t, err, ErrSessionBusy)

		sa.clearActiveIfMatch(sess.ID, racer)
	})

	t.Run("claim fails cleanly, as ErrSessionBusy, if the slot no longer matches", func(t *testing.T) {
		ac := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, ac)
		other := &activeCancel{cancel: func() {}}
		sa.setActiveForTest(sess.ID, other) // something else already took over

		err := sa.summarize(t.Context(), sess.ID, fantasy.ProviderOptions{}, nil, sa.model.Get(), "", nil, ac)
		require.ErrorIs(t, err, ErrSessionBusy)
		got, ok := sa.getActiveForTest(sess.ID)
		require.True(t, ok)
		require.Same(t, other, got, "a failed claim must not disturb whatever is actually installed")

		sa.clearActiveIfMatch(sess.ID, other)
	})
}
