package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
		assert.Equal(t, strings.Repeat("x", 10), kept[0].Parts[0].(message.TextContent).Text,
			"the carried history must respect the budget even for the newest session")
	})

	t.Run("an oversized exchange is replaced by its compact placeholder, not kept whole", func(t *testing.T) {
		t.Parallel()
		// A single delegation whose tool result alone exceeds the budget.
		// Keeping the exchange whole would blow the budget, so it is
		// excluded and replaced by a compact, deterministic placeholder;
		// the result must fit the budget for any sensible budget.
		big := strings.Repeat("x", 5_000)
		huge := []message.Message{
			assistantText("earlier work"),
			toolCallMsg("huge", "read"),
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "huge", Name: "read", Content: big}}},
		}
		kept, dropped := applyCarryOverBudget([][]message.Message{huge}, 100)
		assert.Equal(t, 0, dropped)
		assert.False(t, hasResult(kept, "huge"),
			"the oversized result is dropped, not replayed")
		assert.False(t, hasOrphanToolResult(kept),
			"the placeholder must not leave an orphaned result")
		assert.LessOrEqual(t, messagesTextLen(kept), 100,
			"the placeholder keeps the carried history within budget")
		assert.Contains(t, flattenedText(kept), "tool exchange",
			"the placeholder names that a tool exchange was omitted")
	})

	t.Run("the newest session is trimmed to its tail, not carried whole", func(t *testing.T) {
		t.Parallel()
		long := []message.Message{
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("a", 100)}}},
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("b", 100)}}},
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("c", 10)}}},
		}
		kept, dropped := applyCarryOverBudget([][]message.Message{session("old"), long}, 30)
		assert.Equal(t, 1, dropped)
		require.Len(t, kept, 1,
			"a session that alone blows the budget is carried as its tail, not in full")
		assert.Equal(t, strings.Repeat("c", 10), kept[0].Parts[0].(message.TextContent).Text,
			"the end of the delegation is what the next one follows on from")
	})

	t.Run("the last message survives a budget it cannot fit", func(t *testing.T) {
		t.Parallel()
		huge := []message.Message{
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("a", 100)}}},
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("z", 5_000)}}},
		}
		kept, dropped := applyCarryOverBudget([][]message.Message{huge}, 10)
		assert.Equal(t, 0, dropped)
		require.Len(t, kept, 1, "the newest plain-text tail remains available")
		assert.Equal(t, strings.Repeat("z", 10), kept[0].Parts[0].(message.TextContent).Text)
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

// trimOrphanChecker reports whether a message slice has a tool result whose
// matching tool call is absent (an orphan that preparePrompt would have to
// repair). It is the acceptance criterion for T1: trimming must not produce
// one.
func hasOrphanToolResult(msgs []message.Message) bool {
	knownCalls := make(map[string]struct{})
	for _, m := range msgs {
		if m.Role == message.Assistant {
			for _, tc := range m.ToolCalls() {
				knownCalls[tc.ID] = struct{}{}
			}
		}
	}
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, tr := range m.ToolResults() {
			if _, ok := knownCalls[tr.ToolCallID]; !ok {
				return true
			}
		}
	}
	return false
}

// textMsg, assistantText and toolCallMsg are builders for the trimmed
// histories the T1 tests assert on.
func textMsg(role message.MessageRole, text string) message.Message {
	return message.Message{Role: role, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func assistantText(text string) message.Message {
	return textMsg(message.Assistant, text)
}

func toolCallMsg(id, name string) message.Message {
	return message.Message{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.ToolCall{ID: id, Name: name, Input: "{}", Finished: true}},
	}
}

func toolResultMsg(id string) message.Message {
	return message.Message{
		Role:  message.Tool,
		Parts: []message.ContentPart{message.ToolResult{ToolCallID: id, Name: "tool", Content: "ok"}},
	}
}

// TestTrimToBudgetKeepsExchangesWhole is the central T1 test: trimming a
// history whose budget lands inside a tool exchange must keep every kept
// result together with its call, even across interleaved assistant
// text/reasoning and multiple exchanges, and must never leave an orphaned
// result. An exchange that alone exceeds the budget is excluded and
// replaced by a compact placeholder that keeps the result within budget.
func TestTrimToBudgetKeepsExchangesWhole(t *testing.T) {
	t.Parallel()

	t.Run("parallel calls kept together", func(t *testing.T) {
		t.Parallel()
		// One assistant turn issues two parallel tool calls (a single
		// assistant message carries both ToolCall parts, as the model
		// persists them); the two results are separate tool messages. A
		// naive cut between the two results would orphan the one kept with
		// its call dropped.
		parallel := message.Message{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ToolCall{ID: "call-a", Name: "grep", Input: "{}", Finished: true},
				message.ToolCall{ID: "call-b", Name: "glob", Input: "{}", Finished: true},
			},
		}
		msgs := []message.Message{
			assistantText("thinking"),
			parallel,
			toolResultMsg("call-a"),
			toolResultMsg("call-b"),
			assistantText("done"),
		}
		// Budget small enough that the raw cut lands inside the exchange.
		kept := trimToBudget(msgs, 8)
		assert.False(t, hasOrphanToolResult(kept),
			"trimming must not leave a result whose call was dropped")
		// The parallel batch moves as a unit: call-a and call-b are kept
		// together with both results, or both are dropped with "done".
		if hasResult(kept, "call-a") {
			assert.True(t, hasCall(kept, "call-a"), "call-a kept with its result")
			assert.True(t, hasResult(kept, "call-b"), "the parallel batch is kept whole")
			assert.True(t, hasCall(kept, "call-b"), "call-b kept with its result")
		}
	})

	t.Run("interleaved assistant text between call and result", func(t *testing.T) {
		t.Parallel()
		// The budget cut lands on an assistant *text* message that sits
		// between a tool call and its result. The old code looked only at
		// the Role of the first kept message; that message is assistant, so
		// it would not have moved the cut back to the call, orphaning the
		// result. The ID-based scan must pull the cut back to the call.
		msgs := []message.Message{
			toolCallMsg("c1", "grep"),
			toolResultMsg("c1"),
			assistantText("some reasoning in between"),
			toolCallMsg("c2", "read"),
			toolResultMsg("c2"),
			assistantText("done"),
		}
		for budget := range 60 {
			kept := trimToBudget(msgs, budget)
			assert.False(t, hasOrphanToolResult(kept),
				"budget %d: an interleaved assistant text must not orphan a result", budget)
		}
	})

	t.Run("multiple exchanges, cut inside the second", func(t *testing.T) {
		t.Parallel()
		// Two complete exchanges. The budget is tuned so the raw cut lands
		// between the second exchange's call and its result. The cut must
		// move back to the second exchange's call, keeping that exchange
		// whole, while the first exchange is dropped.
		msgs := []message.Message{
			toolCallMsg("e1", "grep"),
			toolResultMsg("e1"),
			assistantText("in between"),
			toolCallMsg("e2", "read"),
			toolResultMsg("e2"),
			assistantText("done"),
		}
		for budget := range 60 {
			kept := trimToBudget(msgs, budget)
			assert.False(t, hasOrphanToolResult(kept),
				"budget %d: trimming must not leave a result whose call was dropped", budget)
			// e2's result, if kept, must be kept with e2's call.
			if hasResult(kept, "e2") {
				assert.True(t, hasCall(kept, "e2"),
					"budget %d: e2's result must be kept with its call", budget)
			}
		}
	})

	t.Run("cut on an exchange boundary keeps the exchange whole", func(t *testing.T) {
		t.Parallel()
		// Two exchanges; the budget is tuned so the raw cut falls exactly
		// at the boundary between them. The newer exchange must be kept in
		// full (call + result), not chopped.
		msgs := []message.Message{
			toolCallMsg("old", "grep"),
			toolResultMsg("old"),
			toolCallMsg("new", "read"),
			toolResultMsg("new"),
		}
		kept := trimToBudget(msgs, 24)
		assert.True(t, hasCall(kept, "new"), "the newer exchange's call survives")
		assert.True(t, hasResult(kept, "new"), "the newer exchange's result survives")
		assert.False(t, hasOrphanToolResult(kept))
	})

	t.Run("partial results: an interrupted call batch is repaired, not split", func(t *testing.T) {
		t.Parallel()
		// A parallel batch where one result never arrived (interrupted
		// stream). The call is kept; its missing result is an upstream
		// orphan that preparePrompt synthesises - trimming must not create
		// a new one by dropping the call while keeping the other result.
		msgs := []message.Message{
			toolCallMsg("call-a", "grep"),
			toolCallMsg("call-b", "glob"),
			toolResultMsg("call-b"), // call-a's result never came back
			assistantText("done"),
		}
		for budget := range 20 {
			kept := trimToBudget(msgs, budget)
			// call-b's result must never be orphaned by trimming.
			if hasResult(kept, "call-b") {
				assert.True(t, hasCall(kept, "call-b"),
					"budget %d: call-b's result must be kept with its call", budget)
			}
		}
	})

	t.Run("an oversized exchange is excluded and stubbed, and the result fits the budget", func(t *testing.T) {
		t.Parallel()
		// A single exchange whose result alone exceeds the budget. The
		// exchange is excluded in full and replaced by a compact,
		// deterministic placeholder (two text messages: a user note and an
		// assistant acknowledgement). The kept result must fit the budget
		// for any sensible budget and must not leave an orphan.
		big := strings.Repeat("x", 5_000)
		msgs := []message.Message{
			assistantText("earlier work"),
			toolCallMsg("huge", "read"),
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "huge", Name: "read", Content: big}}},
		}
		const budget = 100
		kept := trimToBudget(msgs, budget)
		assert.False(t, hasResult(kept, "huge"),
			"the oversized result is dropped, not replayed")
		assert.False(t, hasOrphanToolResult(kept), "the stub must not leave an orphan")
		assert.LessOrEqual(t, messagesTextLen(kept), budget,
			"the placeholder keeps the carried history within budget")
		assert.Contains(t, flattenedText(kept), "tool exchange",
			"the placeholder names that a tool exchange was omitted")
		// The placeholder is one budget-aware plain-text message.
		require.Len(t, kept, 1, "the placeholder is one text note")
		assert.Equal(t, message.User, kept[0].Role, "the placeholder is a user text note")
		for _, part := range kept[0].Parts {
			_, isToolCall := part.(message.ToolCall)
			assert.False(t, isToolCall, "the placeholder must not contain synthetic tool calls")
		}
	})
}

// flattenedText joins the TextContent of every message, for containment
// assertions on the placeholder and carried history.
func flattenedText(msgs []message.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		for _, part := range m.Parts {
			if tc, ok := part.(message.TextContent); ok {
				b.WriteString(tc.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// hasCall / hasResult check a message slice for a tool call or result by id.
func hasCall(msgs []message.Message, id string) bool {
	for _, m := range msgs {
		for _, tc := range m.ToolCalls() {
			if tc.ID == id {
				return true
			}
		}
	}
	return false
}

func hasResult(msgs []message.Message, id string) bool {
	for _, m := range msgs {
		for _, tr := range m.ToolResults() {
			if tr.ToolCallID == id {
				return true
			}
		}
	}
	return false
}

// TestTrimToBudgetNoRegressionOnPlainText pins that the exchange-aware cut
// behaves like the old message-granular cut for histories with no tool
// calls: it keeps the newest messages that fit, drops the rest, and a tail
// that alone exceeds the budget is still kept (never an empty result).
func TestTrimToBudgetExchangeBoundariesAndPlaceholder(t *testing.T) {
	t.Parallel()

	t.Run("membership follows IDs across assistant text", func(t *testing.T) {
		calls := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "a", Name: "read", Input: "{}", Finished: true},
			message.ToolCall{ID: "b", Name: "glob", Input: "{}", Finished: true},
		}}
		msgs := []message.Message{
			calls,
			assistantText("reasoning between calls and results"),
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "a", Content: strings.Repeat("a", 100)}}},
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "b", Content: strings.Repeat("b", 100)}}},
			assistantText("final plain text tail"),
		}
		kept := trimToBudget(msgs, len("final plain text tail"))
		require.Len(t, kept, 1)
		assert.Equal(t, "final plain text tail\n", flattenedText(kept))
		assert.False(t, hasOrphanToolResult(kept))
	})

	t.Run("placeholder is budget-aware text only", func(t *testing.T) {
		calls := make([]message.ContentPart, 0, 20)
		for i := range 20 {
			calls = append(calls, message.ToolCall{ID: fmt.Sprintf("id-%d", i), Name: "very-long-tool-name", Input: "{}", Finished: true})
		}
		msgs := []message.Message{
			{Role: message.Assistant, Parts: calls},
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "id-0", Content: strings.Repeat("x", 100)}}},
		}
		for _, budget := range []int{0, 1, 10} {
			kept := trimToBudget(msgs, budget)
			assert.LessOrEqual(t, messagesTextLen(kept), budget)
			for _, m := range kept {
				for _, part := range m.Parts {
					_, isCall := part.(message.ToolCall)
					assert.False(t, isCall)
				}
			}
		}
	})
}

func TestHasOrphanResultMatchesPreparePromptRoles(t *testing.T) {
	t.Parallel()

	t.Run("call outside assistant role cannot satisfy tool result", func(t *testing.T) {
		msgs := []message.Message{
			{Role: message.User, Parts: []message.ContentPart{message.ToolCall{ID: "shared", Name: "read"}}},
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "shared", Content: "result"}}},
		}
		assert.True(t, hasOrphanResult(msgs))
	})

	t.Run("result outside tool role cannot satisfy assistant call", func(t *testing.T) {
		msgs := []message.Message{
			{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "shared", Name: "read"}}},
			{Role: message.User, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "shared", Content: "result"}}},
		}
		assert.False(t, hasOrphanResult(msgs), "only tool-role results can be orphaned")
	})
}

func TestTrimToBudgetLogsActualCounts(t *testing.T) {
	// captureLogs serializes access to the process logger.
	logs := captureLogs(t)

	tests := []struct {
		name                           string
		msgs                           []message.Message
		budget                         int
		dropped, kept, truncated, stub int
	}{
		{
			name:    "short placeholder before a tail",
			msgs:    []message.Message{toolCallMsg("call", "read"), {Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Content: strings.Repeat("x", 100)}}}, assistantText("tail")},
			budget:  len("tail") + 5,
			dropped: 2, kept: 1, stub: 1,
		},
		{
			name:    "placeholder replaces final exchange",
			msgs:    []message.Message{toolCallMsg("call", "read"), {Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call", Content: strings.Repeat("x", 100)}}}},
			budget:  10,
			dropped: 2, stub: 1,
		},
		{
			name:    "truncated plain text",
			msgs:    []message.Message{assistantText("old"), assistantText(strings.Repeat("z", 100))},
			budget:  10,
			dropped: 1, truncated: 1,
		},
		{
			name:    "ordinary user text with placeholder prefix",
			msgs:    []message.Message{assistantText("old"), {Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Earlier tool exchange omitted: user note"}}}},
			budget:  len("Earlier tool exchange omitted: user note"),
			dropped: 1, kept: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := len(logs.String())
			kept := trimToBudget(test.msgs, test.budget)
			record := logs.String()[before:]
			assert.LessOrEqual(t, messagesTextLen(kept), test.budget)
			assert.Contains(t, record, fmt.Sprintf("dropped_messages=%d", test.dropped))
			assert.Contains(t, record, fmt.Sprintf("kept_messages=%d", test.kept))
			assert.Contains(t, record, fmt.Sprintf("truncated_messages=%d", test.truncated))
			assert.Contains(t, record, fmt.Sprintf("placeholder_messages=%d", test.stub))
		})
	}
}

func TestTrimToBudgetUTF8Boundaries(t *testing.T) {
	t.Parallel()

	t.Run("oversized plain text keeps the maximum valid suffix", func(t *testing.T) {
		const budget = 10
		text := "префикс🙂конец"
		kept := trimToBudget([]message.Message{{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: text}}}}, budget)
		require.Len(t, kept, 1)
		got := kept[0].Parts[0].(message.TextContent).Text
		assert.Equal(t, "конец", got, "the suffix is the longest valid UTF-8 suffix within ten bytes")
		assert.LessOrEqual(t, len(got), budget)
		assert.True(t, utf8.ValidString(got))
	})

	t.Run("oversized placeholder is valid UTF-8 and serializable", func(t *testing.T) {
		msgs := []message.Message{
			{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "id", Name: "инструмент🙂", Input: "{}"}}},
			{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "id", Content: strings.Repeat("результат🙂", 20)}}},
		}
		for _, budget := range []int{1, 2, 3, 7, 10} {
			kept := trimToBudget(msgs, budget)
			assert.LessOrEqual(t, messagesTextLen(kept), budget)
			for _, m := range kept {
				for _, part := range m.Parts {
					text, ok := part.(message.TextContent)
					require.True(t, ok, "placeholder is text only")
					assert.True(t, utf8.ValidString(text.Text))
				}
			}
			_, err := json.Marshal(kept)
			require.NoError(t, err, "the placeholder remains safe for message serialization")
		}
	})
}

func TestTrimToBudgetNoRegressionOnPlainText(t *testing.T) {
	t.Parallel()

	t.Run("keeps every consecutive tail message that fits", func(t *testing.T) {
		t.Parallel()
		long := []message.Message{
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("a", 100)}}},
			{Parts: []message.ContentPart{message.TextContent{Text: "first"}}},
			{Parts: []message.ContentPart{message.TextContent{Text: "second"}}},
			{Parts: []message.ContentPart{message.TextContent{Text: "third"}}},
		}
		kept := trimToBudget(long, len("firstsecondthird"))
		require.Len(t, kept, 3)
		assert.Equal(t, "first\nsecond\nthird\n", flattenedText(kept))
	})

	t.Run("a plain-text tail that alone exceeds the budget is bounded, never empty", func(t *testing.T) {
		t.Parallel()
		// A single oversized text message. trimToBudget cannot stub plain
		// text (there is no tool exchange to exclude), so it keeps the
		// newest message rather than returning nothing - the same contract
		// applyCarryOverBudget has for the newest session. The result may
		// exceed the budget, but it is the most recent context and
		// dropping it would lose exactly what the next delegation follows
		// on from.
		huge := []message.Message{
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("a", 100)}}},
			{Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("z", 5_000)}}},
		}
		kept := trimToBudget(huge, 10)
		require.Len(t, kept, 1, "dropping the tail entirely would lose the most recent context")
		assert.Equal(t, strings.Repeat("z", 10), kept[0].Parts[0].(message.TextContent).Text)
	})
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

// TestTrimToBudgetPreparePromptProducesNoOrphanRepairs is the T1
// acceptance test, run through the real trim → preparePrompt path. The
// budget is tuned so the raw cut lands inside a tool exchange; after
// trimToBudget the kept history must be self-consistent, so preparePrompt
// neither drops an orphaned result nor injects a synthetic one. (The
// general repair counter in compat.go is not expected to reach zero -
// interrupted streams and manual cancels still produce orphans - but the
// specific orphans that trimming used to create must not appear here.)
func TestTrimToBudgetPreparePromptProducesNoOrphanRepairs(t *testing.T) {
	// No t.Parallel(): captureLogs serializes against every other log
	// capture in the package through logCaptureMu.
	logs := captureLogs(t)

	env := testEnv(t)
	model := newPromptCapturingModel("fake", "fake-model")
	sa := testSessionAgent(env, model, "system")
	agent, ok := sa.(*sessionAgent)
	require.True(t, ok, "testSessionAgent must return a *sessionAgent")

	// A prior session whose history, trimmed to a small budget, would land
	// the cut between an assistant tool call and one of its results.
	// assistant(call-a, call-b) + result(a) + result(b) + "done".
	session := []message.Message{
		assistantText("thinking"),
		{Role: message.Assistant, Parts: []message.ContentPart{
			message.ToolCall{ID: "call-a", Name: "grep", Input: "{}", Finished: true},
			message.ToolCall{ID: "call-b", Name: "glob", Input: "{}", Finished: true},
		}},
		toolResultMsg("call-a"),
		toolResultMsg("call-b"),
		assistantText("done"),
	}

	for _, budget := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 15, 20, 30, 40} {
		kept := trimToBudget(session, budget)
		assert.False(t, hasOrphanToolResult(kept),
			"budget %d: trimming must not leave a result whose call was dropped", budget)

		_, _ = agent.preparePrompt(kept, true, nil)
	}
	// The trimming orphans that the old code produced are repaired by
	// preparePrompt with a warning; T1 must not create any of them.
	assert.NotContains(t, logs.String(), "Dropping orphaned tool result",
		"trimToBudget must not leave a result whose call was dropped for preparePrompt to repair")
	assert.NotContains(t, logs.String(), "Injecting synthetic tool result",
		"trimToBudget must not leave a call whose result was dropped for preparePrompt to repair")
}
