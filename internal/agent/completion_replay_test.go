package agent

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

func TestCompletionReplayDeduplicatesWithinAndAcrossSteps(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &promptRecordingModel{text: "done"}, "system").(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sess.ID}}
	completion := TaskCompletion{DelegationID: "task", Status: "completed", ResultText: "first result", TerminalAt: time.Unix(123, 1)}
	sa.enqueueCompletion(sess.ID, completion)
	sa.enqueueCompletion(sess.ID, completion)
	prompt, pending, err := turn.foldCompletions(t.Context(), nil, 0)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.Len(t, prompt, 1)
	sa.enqueueCompletion(sess.ID, completion)
	prompt, _, err = turn.foldCompletions(t.Context(), nil, 1)
	require.NoError(t, err)
	require.Len(t, prompt, 1)
	completion.TerminalAt = time.Unix(123, 2)
	completion.ResultText = "second result"
	sa.enqueueCompletion(sess.ID, completion)
	prompt, pending, err = turn.foldCompletions(t.Context(), nil, 2)
	require.NoError(t, err)
	require.Len(t, prompt, 2)
	turn.pendingCompletions = pending
	turn.requeuePendingCompletions()
	prompt, _, err = turn.foldCompletions(t.Context(), nil, 3)
	require.NoError(t, err)
	require.Len(t, prompt, 2)
	stored, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
}

func TestCompletionReplayUsesEffectiveHistorySnapshot(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	sa := testSessionAgent(env, &promptRecordingModel{text: "done"}, "system").(*sessionAgent)
	sess, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	completion := TaskCompletion{DelegationID: "task", Status: "completed", ResultText: "retained result", TerminalAt: time.Unix(123, 1)}
	first := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sess.ID}}
	sa.enqueueCompletion(sess.ID, completion)
	_, _, err = first.foldCompletions(t.Context(), nil, 0)
	require.NoError(t, err)
	stored, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, message.OriginAgent, stored[0].Origin)
	for _, present := range []bool{true, false} {
		turn := &runTurn{agent: sa, call: SessionAgentCall{SessionID: sess.ID}, historyMessageIDs: map[string]struct{}{}}
		if present {
			turn.historyMessageIDs[stored[0].ID] = struct{}{}
		}
		sa.enqueueCompletion(sess.ID, completion)
		prompt, pending, err := turn.foldCompletions(t.Context(), nil, 0)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		if present {
			require.Empty(t, prompt)
		} else {
			require.Len(t, prompt, 1)
		}
	}
	stored, err = env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
}
