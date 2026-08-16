package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// carryOverProviderCfg mirrors the provider stub the other runSubAgent
// tests use; runSubAgent needs a configured provider to reach the run.
var carryOverProviderCfg = config.ProviderConfig{ID: "test-provider"}

// recordingSubAgent is a mock sub-agent that both records what it was
// handed and writes an exchange into its own session, the way a real run
// would. Recording alone is not enough for these tests: the whole
// question is whether delegation N+1 sees what delegation N left behind,
// so the mock has to actually leave something behind.
type recordingSubAgent struct {
	*mockSessionAgent

	msgs  message.Service
	reply string

	// prior is what the most recent call carried in PriorMessages.
	prior []message.Message
}

func newRecordingSubAgent(env fakeEnv, reply string) *recordingSubAgent {
	r := &recordingSubAgent{msgs: env.messages, reply: reply}
	r.mockSessionAgent = newMockAgent(carryOverProviderCfg.ID, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		r.prior = call.PriorMessages
		if _, err := r.msgs.Create(ctx, call.SessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: call.Prompt}},
		}); err != nil {
			return nil, err
		}
		if _, err := r.msgs.Create(ctx, call.SessionID, message.CreateMessageParams{
			Role:  message.Assistant,
			Parts: []message.ContentPart{message.TextContent{Text: r.reply}},
		}); err != nil {
			return nil, err
		}
		return agentResultWithText(r.reply), nil
	})
	return r
}

// priorText flattens the recorded PriorMessages into one string for
// containment assertions.
func (r *recordingSubAgent) priorText() string {
	var b strings.Builder
	for _, m := range r.prior {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok {
				b.WriteString(tc.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// delegate runs one delegation through runSubAgent. Each call needs its
// own message/tool-call ids: those are what the child session id is built
// from, and reusing them would collide on the session insert.
func delegate(t *testing.T, coord *coordinator, agent SessionAgent, parentSessionID, agentID, nonce, prompt string) {
	t.Helper()
	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent:          agent,
		SessionID:      parentSessionID,
		AgentMessageID: "msg-" + nonce,
		ToolCallID:     "call-" + nonce,
		Prompt:         prompt,
		SessionTitle:   "Delegation",
		AgentID:        agentID,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "delegation failed: %s", resp.Content)
}

// TestRunSubAgent_NamedAgentCarriesItsEarlierConversation is the central
// regression test for this feature: a named agent delegated to twice
// under the same parent must see the first exchange on the second call.
// Before the fix it saw nothing - every delegation created a fresh
// session (the child session id is derived from the message and tool-call
// ids, so it is necessarily new every time) and nothing replayed what
// came before.
func TestRunSubAgent_NamedAgentCarriesItsEarlierConversation(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agent := newRecordingSubAgent(env, "first answer")
	delegate(t, coord, agent, parent.ID, "developer", "1", "first task")
	require.Empty(t, agent.prior, "the very first delegation has nothing to carry over")

	delegate(t, coord, agent, parent.ID, "developer", "2", "second task")

	carried := agent.priorText()
	assert.Contains(t, carried, "first task",
		"the second delegation must see the prompt the first one was given")
	assert.Contains(t, carried, "first answer",
		"the second delegation must see what it answered the first time")
	assert.NotContains(t, carried, "second task",
		"the current delegation's own prompt is not carried history; it arrives as the prompt")
}

// TestRunSubAgent_CarryOverIsScopedToTheParentSession pins the property
// the feature was actually asked for: continuity inside a thread. A
// thread runs on its own session, so scoping carry-over by parent session
// is what keeps one thread's conversation with an agent out of another's.
func TestRunSubAgent_CarryOverIsScopedToTheParentSession(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)

	threadA, err := env.sessions.Create(t.Context(), "Thread A")
	require.NoError(t, err)
	threadB, err := env.sessions.Create(t.Context(), "Thread B")
	require.NoError(t, err)

	agent := newRecordingSubAgent(env, "answer in A")
	delegate(t, coord, agent, threadA.ID, "developer", "a1", "task in thread A")

	delegate(t, coord, agent, threadB.ID, "developer", "b1", "task in thread B")
	assert.Empty(t, agent.prior,
		"a delegation in one thread must not inherit the same agent's conversation from another")

	delegate(t, coord, agent, threadA.ID, "developer", "a2", "another task in thread A")
	assert.Contains(t, agent.priorText(), "task in thread A",
		"back in thread A, the agent must still remember thread A")
	assert.NotContains(t, agent.priorText(), "task in thread B",
		"thread B's exchange must not leak into thread A")
}

// TestRunSubAgent_AnonymousDelegationStaysStateless covers the deliberate
// exclusion: the built-in `agent` and `agentic_fetch` tools pass no agent
// id. They are one-off and frequently run in parallel on unrelated work,
// so stitching their calls into one growing conversation would be a
// regression, not a feature. The named sibling in the same parent also
// proves the empty agent id doesn't match "every child session".
func TestRunSubAgent_AnonymousDelegationStaysStateless(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	named := newRecordingSubAgent(env, "named answer")
	delegate(t, coord, named, parent.ID, "developer", "named", "named task")

	anon := newRecordingSubAgent(env, "anonymous answer")
	delegate(t, coord, anon, parent.ID, "", "anon1", "first anonymous task")
	delegate(t, coord, anon, parent.ID, "", "anon2", "second anonymous task")

	assert.Empty(t, anon.prior,
		"an anonymous delegation must start from nothing, even after an earlier anonymous delegation under the same parent")
}

// TestRunSubAgent_NamedAgentsDoNotShareEachOthersContext: continuity is
// per agent, not per parent. Two roles working under one parent each keep
// their own thread of conversation.
func TestRunSubAgent_NamedAgentsDoNotShareEachOthersContext(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	dev := newRecordingSubAgent(env, "developer answer")
	delegate(t, coord, dev, parent.ID, "developer", "d1", "developer task")

	reviewer := newRecordingSubAgent(env, "reviewer answer")
	delegate(t, coord, reviewer, parent.ID, "reviewer", "r1", "reviewer task")
	assert.Empty(t, reviewer.prior,
		"the reviewer must not inherit the developer's conversation")

	delegate(t, coord, dev, parent.ID, "developer", "d2", "developer follow-up")
	assert.Contains(t, dev.priorText(), "developer task")
	assert.NotContains(t, dev.priorText(), "reviewer task",
		"the developer must not inherit the reviewer's conversation")
}

// TestRunSubAgent_SummarizedPriorSessionCarriesOnlyItsSummaryOnward proves
// carried history honours a prior session's own compaction rather than
// replaying what that session already decided to forget - the claim
// trimToSummary's shared use makes.
func TestRunSubAgent_SummarizedPriorSessionCarriesOnlyItsSummaryOnward(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agent := newRecordingSubAgent(env, "answer before the summary")
	delegate(t, coord, agent, parent.ID, "developer", "1", "ancient detail")

	// Summarize the child session the way sessionAgent.Summarize would:
	// append a summary message and point the session at it.
	childID := env.sessions.CreateAgentToolSessionID("msg-1", "call-1")
	summary, err := env.messages.Create(t.Context(), childID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "the summary of what happened"}},
	})
	require.NoError(t, err)
	child, err := env.sessions.Get(t.Context(), childID)
	require.NoError(t, err)
	child.SummaryMessageID = summary.ID
	_, err = env.sessions.Save(t.Context(), child)
	require.NoError(t, err)

	delegate(t, coord, agent, parent.ID, "developer", "2", "next task")

	carried := agent.priorText()
	assert.Contains(t, carried, "the summary of what happened",
		"the summary must be carried over")
	assert.NotContains(t, carried, "ancient detail",
		"messages the prior session summarized away must not be replayed")
}

// TestApplyCarryOverBudget covers the bound on how much a long-lived
// agent may drag along.
func TestApplyCarryOverBudget(t *testing.T) {
	t.Parallel()

	session := func(text string) []message.Message {
		return []message.Message{{Parts: []message.ContentPart{message.TextContent{Text: text}}}}
	}

	t.Run("everything fits", func(t *testing.T) {
		t.Parallel()
		kept, dropped := applyCarryOverBudget([][]message.Message{session("aa"), session("bb")}, 100)
		assert.Equal(t, 0, dropped)
		assert.Len(t, kept, 2)
	})

	t.Run("oldest whole sessions are dropped first", func(t *testing.T) {
		t.Parallel()
		kept, dropped := applyCarryOverBudget([][]message.Message{
			session(strings.Repeat("o", 10)),
			session(strings.Repeat("m", 10)),
			session(strings.Repeat("n", 10)),
		}, 25)
		assert.Equal(t, 1, dropped, "only the oldest session should not fit")
		require.Len(t, kept, 2)
		assert.Contains(t, kept[0].Parts[0].(message.TextContent).Text, "m")
		assert.Contains(t, kept[1].Parts[0].(message.TextContent).Text, "n")
	})

	t.Run("the newest session survives even when it alone exceeds the budget", func(t *testing.T) {
		t.Parallel()
		kept, dropped := applyCarryOverBudget([][]message.Message{
			session("old"),
			session(strings.Repeat("x", 500)),
		}, 10)
		assert.Equal(t, 1, dropped)
		require.Len(t, kept, 1)
		assert.Equal(t, strings.Repeat("x", 500), kept[0].Parts[0].(message.TextContent).Text,
			"dropping the most recent exchange would lose exactly the context the next delegation follows on from")
	})

	t.Run("nothing to carry", func(t *testing.T) {
		t.Parallel()
		kept, dropped := applyCarryOverBudget(nil, 10)
		assert.Nil(t, kept)
		assert.Equal(t, 0, dropped)
	})
}

// TestRunSubAgent_CarryOverStaysWithinBudget exercises the budget through
// the real path: enough prior delegations to blow past
// maxCarriedSubAgentChars must not hand the next one an unbounded
// transcript.
func TestRunSubAgent_CarryOverStaysWithinBudget(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := newTestCoordinator(t, env, carryOverProviderCfg)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	// Each delegation leaves behind roughly a third of the budget, so the
	// carried history has to start shedding the oldest ones by the fifth.
	bulk := strings.Repeat("z", maxCarriedSubAgentChars/3)
	agent := newRecordingSubAgent(env, bulk)
	for i := range 5 {
		delegate(t, coord, agent, parent.ID, "developer", fmt.Sprint(i), fmt.Sprintf("task %d: %s", i, bulk))
	}

	assert.LessOrEqual(t, len(agent.priorText()), 2*maxCarriedSubAgentChars,
		"carried history must stay bounded; the budget drops whole sessions, so the last one kept may overshoot, but not without limit")
	assert.NotContains(t, agent.priorText(), "task 0:",
		"the oldest delegations must be shed once the budget is exceeded")
	assert.Contains(t, agent.priorText(), "task 3:",
		"the most recent delegations must survive")
}

// TestPriorMessagesReachTheModelAndAreNotPersisted is the other half of
// the contract, asserted where it is actually observable: carried history
// must arrive in the prompt the model sees, and must stay out of the
// session's own message store - the sessions it came from own it.
func TestPriorMessagesReachTheModelAndAreNotPersisted(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	model := newPromptCapturingModel("fake", "fake-model")
	sa := testSessionAgent(env, model, "system")

	sess, err := env.sessions.Create(t.Context(), "Delegation")
	require.NoError(t, err)

	result, err := sa.Run(t.Context(), SessionAgentCall{
		SessionID: sess.ID,
		Prompt:    "the new task",
		PriorMessages: []message.Message{
			{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "what we discussed before"}}},
			{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "what I answered before"}}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	calls := model.calls()
	require.Len(t, calls, 1)
	assert.True(t, promptHasUserText(calls[0], "what we discussed before"),
		"carried history must reach the model")
	assert.True(t, promptHasRoleText(calls[0], fantasy.MessageRoleAssistant, "what I answered before"),
		"carried history must keep its roles, so the agent recognises its own earlier answers")

	msgs, err := env.messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	assert.False(t, containsRoleText(msgs, message.User, "what we discussed before"),
		"carried history must not be copied into this session's store")

	// Title generation keys off this session's own messages, so carried
	// history must not make the delegation look like it already has a
	// conversation and skip titling.
	select {
	case <-model.titleGot:
	case <-time.After(10 * time.Second):
		t.Fatal("carried history suppressed title generation for a session that had none of its own")
	}
}
