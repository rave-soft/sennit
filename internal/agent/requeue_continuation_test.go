package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRequeueContinuation_SurvivesUnrelatedSiblingCancel is a focused,
// dispatcher-level regression test for the acceptSeq == 0 bug:
// requeueContinuation used to append the continuation call as-is, without
// stripping its (already Closed) Accepted handle or stamping a fresh
// acceptSeq, so it carried acceptSeq == 0 into the queue. canceledBySeq
// treats 0 as "untracked" and therefore covered by *any* pending cancel
// mark for the session - so a Cancel that had nothing to do with this
// call (here, one that lands on an unrelated sibling accept while
// "original" is busy) silently dropped the continuation anyway, and the
// user's turn stopped dead after a summarize.
//
// This drives requeueContinuation and drainNext directly, without a real
// model or a full Run, so the accept-sequence bookkeeping is exercised in
// isolation from the streaming machinery TestRun_AutoSummarize
// ContinuationPreservesAcceptedSequence (summarize_race_test.go) checks
// end to end.
func TestRequeueContinuation_SurvivesUnrelatedSiblingCancel(t *testing.T) {
	t.Parallel()
	sa, _ := newCancelTestAgent(t)
	sessionID := "sess"

	// "original" is accepted and becomes this session's active run, the
	// way dispatchDecision's idle branch would: BeginAccepted, then
	// Close on entry to Stream.
	original := sa.BeginAccepted(sessionID)
	sa.setActiveForTest(sessionID, &activeCancel{cancel: func() {}})
	original.Close()

	// A sibling accept, unrelated to "original", is outstanding when a
	// Cancel arrives for this session. Cancel() records a pending cancel
	// mark covering it (acceptedRuns > 0), and - because the message
	// queue is still empty at this point - has nothing else to drop.
	sibling := sa.BeginAccepted(sessionID)
	sa.Cancel(sessionID)
	require.True(t, sa.hasPendingCancel(sessionID), "the sibling's cancel must record a pending mark")

	// finishTurn's summarize tail requeues "original"'s continuation only
	// after that cancel has already resolved.
	sa.requeueContinuation(SessionAgentCall{
		SessionID: sessionID,
		RunID:     "original",
		Accepted:  original,
	}, func() {})

	// sibling stays open (not Closed) across the check below: closing it
	// first would drop acceptedRuns to 0, which itself clears the cancel
	// mark (see endAccepted) and would pass this test regardless of
	// whether the fix is present. Keeping it open is what keeps the mark
	// - and therefore the bug this guards against - actually live.
	_, next, canceledDrops := sa.drainNext(sessionID)
	sibling.Close()
	require.Empty(t, canceledDrops, "the continuation must not be reported as a canceled drop")
	require.NotNil(t, next, "an unrelated sibling's cancel must not drop the post-summary continuation")
	require.Equal(t, "original", next.RunID)
}
