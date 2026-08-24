package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/stretchr/testify/require"
)

// repairLine returns the first captured JSON log line whose msg is the given
// repair message AND that matches the supplied predicate (used to scope the
// line to this test's session or message, since captureJSONLogs redirects the
// process-wide slog default and parallel tests in the package may emit their
// own repair lines). The predicate receives the decoded line; it returns true
// for the line this test wants.
func repairLine(t *testing.T, buf *syncLogBuffer, msg string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &decoded))
		if decoded["msg"] == msg && match(decoded) {
			return decoded
		}
	}
	return nil
}

// matchSessionID builds a repairLine predicate that scopes to a session id. A
// session id is a UUID, so this isolates the test's own lines from every other
// test in the package (including the pre-existing orphan tests, which log with
// an empty session id).
func matchSessionID(sessionID string) func(map[string]any) bool {
	return func(line map[string]any) bool { return line["session_id"] == sessionID }
}

// matchMessageID builds a repairLine predicate that scopes to a message id.
// Used by the no-options test, which has no session id to filter on, so it
// instead scopes to the one message it created.
func matchMessageID(messageID string) func(map[string]any) bool {
	return func(line map[string]any) bool { return line["message_id"] == messageID }
}

// seedOrphanedCallHistory builds a session whose history ends with an
// assistant message carrying a tool call that has no matching result - the
// interrupted-stream orphan. The message ids are returned so the test can
// assert the repair line localizes to the right message.
func seedOrphanedCallHistory(t *testing.T, env fakeEnv) (sessionID, assistantMsgID string) {
	t.Helper()
	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "repair-orphan-call")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do a thing"}},
	})
	require.NoError(t, err)

	assistant, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "checking"},
			message.ToolCall{
				ID:       "call_orphan",
				Name:     "bash",
				Input:    `{"command":"echo SECRET_TOOL_INPUT_MUST_NOT_LEAK"}`,
				Finished: true,
			},
		},
	})
	require.NoError(t, err)
	return sess.ID, assistant.ID
}

// seedOrphanedResultHistory builds a session whose history has a tool *result*
// whose matching tool *call* was dropped (the drop_result orphan): an assistant
// message with a call, then a tool result, then the call's assistant message is
// absent because a later message dropped it. The simplest faithful shape is a
// tool message whose result references a call id that appears nowhere.
func seedOrphanedResultHistory(t *testing.T, env fakeEnv) (sessionID, toolMsgID string) {
	t.Helper()
	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "repair-orphan-result")
	require.NoError(t, err)

	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do a thing"}},
	})
	require.NoError(t, err)

	// A tool result whose tool_call_id has no matching tool call anywhere in
	// the history (the call's assistant message was dropped). The result's
	// content deliberately contains a secret that must not be logged.
	tool, err := env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_dropped",
				Name:       "read",
				Content:    "SECRET_RESULT_CONTENT_MUST_NOT_LEAK",
			},
		},
	})
	require.NoError(t, err)
	return sess.ID, tool.ID
}

// TestRepairDiag_InjectResultFields proves the orphan-repair diagnostic for
// the interrupted-stream orphan (an orphaned tool call): the line carries the
// session/run/message ids, the carried origin, the repair action
// inject_result, the tool_call_id and name, the interrupted=true flag, and the
// running injected counter - and it leaks no tool input or message content.
func TestRepairDiag_InjectResultFields(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessionID, assistantMsgID := seedOrphanedCallHistory(t, env)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID(sessionID, "run-repair-inject"),
		withRepairOrigins(originSlice(msgs, originPersisted)),
	)

	line := repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(sessionID))
	require.NotNil(t, line, "the orphaned tool call must produce an inject_result repair line")

	require.Equal(t, sessionID, line["session_id"])
	require.Equal(t, "run-repair-inject", line["run_id"])
	require.Equal(t, assistantMsgID, line["message_id"],
		"the repair line must localize to the assistant message that carries the orphaned call")
	require.Equal(t, string(originPersisted), line["origin"])
	require.Equal(t, repairInjectResult, line["repair_action"])
	require.Equal(t, "call_orphan", line["tool_call_id"])
	require.Equal(t, "bash", line["tool_name"])
	require.Equal(t, true, line["interrupted"],
		"an orphaned call is the interrupted-stream signature, so interrupted must be true")
	// The counter is present and is a positive integer total.
	total, ok := line["orphan_repair_injected_total"].(float64)
	require.True(t, ok, "the line must carry the running injected-repair counter")
	require.GreaterOrEqual(t, total, 1.0)

	// No secret may leak: neither the tool input nor the message text.
	raw := logs.String()
	require.NotContains(t, raw, "SECRET_TOOL_INPUT_MUST_NOT_LEAK",
		"the repair line must not log the tool call input")
	require.NotContains(t, raw, "checking", "the repair line must not log the message text")
}

// TestRepairDiag_DropResultFields proves the orphan-repair diagnostic for the
// dropped-call orphan (an orphaned tool result): repair_action is drop_result,
// interrupted is false (a stale result is not an interrupted stream), and the
// line carries the ids, origin, tool_call_id, the tool name resolved from the
// original message, and the running dropped counter.
func TestRepairDiag_DropResultFields(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessionID, toolMsgID := seedOrphanedResultHistory(t, env)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID(sessionID, "run-repair-drop"),
		withRepairOrigins(originSlice(msgs, originPersisted)),
	)

	line := repairLine(t, logs, "Dropping orphaned tool result with no matching tool call", matchSessionID(sessionID))
	require.NotNil(t, line, "the orphaned tool result must produce a drop_result repair line")

	require.Equal(t, sessionID, line["session_id"])
	require.Equal(t, "run-repair-drop", line["run_id"])
	require.Equal(t, toolMsgID, line["message_id"],
		"the repair line must localize to the tool message that carries the orphaned result")
	require.Equal(t, string(originPersisted), line["origin"])
	require.Equal(t, repairDropResult, line["repair_action"])
	require.Equal(t, "call_dropped", line["tool_call_id"])
	require.Equal(t, "read", line["tool_name"],
		"the tool name is resolved from the original message, not the fantasy part")
	require.Equal(t, false, line["interrupted"],
		"a dropped result is a stale orphan, not an interrupted stream")
	total, ok := line["orphan_repair_dropped_total"].(float64)
	require.True(t, ok, "the line must carry the running dropped-repair counter")
	require.GreaterOrEqual(t, total, 1.0)

	raw := logs.String()
	require.NotContains(t, raw, "SECRET_RESULT_CONTENT_MUST_NOT_LEAK",
		"the repair line must not log the tool result content")
}

// TestRepairDiag_CarriedVsPersistedOrigin proves the origin is really carried
// per-message, not guessed from position: a single preparePrompt call over a
// combined history (carried messages prepended in front of the session's own)
// repairs one orphan in each half, and each line's origin names the right
// source. This is the "протащить origin реально, не угадывать" guarantee.
func TestRepairDiag_CarriedVsPersistedOrigin(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	ctx := t.Context()
	// The session's own history has an orphaned *call* (interrupted stream).
	sessionID, ownOrphanMsgID := seedOrphanedCallHistory(t, env)
	ownMsgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	// A carried history from another session has an orphaned *result*
	// (dropped call). It is a different session so its ids cannot collide.
	carried, err := env.sessions.Create(ctx, "repair-carried")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, carried.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "carried seed"}},
	})
	require.NoError(t, err)
	carriedOrphanMsgID := ""
	cm, err := env.messages.Create(ctx, carried.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID: "call_carried_dropped",
				Name:       "glob",
				Content:    "carried result",
			},
		},
	})
	require.NoError(t, err)
	carriedOrphanMsgID = cm.ID
	carriedMsgs, err := env.messages.List(ctx, carried.ID)
	require.NoError(t, err)

	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	// Combined: carried in front, own after - exactly what the turn path does.
	combined := append(append([]message.Message{}, carriedMsgs...), ownMsgs...)
	_, _ = agent.preparePrompt(combined, true, nil, nil,
		withRepairSessionID(sessionID, "run-repair-mixed"),
		withRepairOrigins(turnOrigins(carriedMsgs, ownMsgs)),
	)

	inject := repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(sessionID))
	require.NotNil(t, inject, "the carried + own orphaned call must be repaired")
	require.Equal(t, ownOrphanMsgID, inject["message_id"])
	require.Equal(t, string(originPersisted), inject["origin"],
		"the orphan in the session's own history must be localized as persisted, not carried")

	drop := repairLine(t, logs, "Dropping orphaned tool result with no matching tool call", matchSessionID(sessionID))
	require.NotNil(t, drop, "the carried orphaned result must be repaired")
	require.Equal(t, carriedOrphanMsgID, drop["message_id"])
	require.Equal(t, string(originCarried), drop["origin"],
		"the orphan in the carried-in history must be localized as carried, not persisted")
	// Both lines share the turn's run id.
	require.Equal(t, "run-repair-mixed", inject["run_id"])
	require.Equal(t, "run-repair-mixed", drop["run_id"])
}

// TestRepairDiag_SummaryOrigin proves the summarize pass tags its repairs as
// origin=summary: a summarize over a session whose history contains an
// orphaned call logs the repair with the summary origin, not persisted.
func TestRepairDiag_SummaryOrigin(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessionID, _ := seedOrphanedCallHistory(t, env)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	ctx := t.Context()
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	// Drive preparePrompt exactly the way usage.go's summarize does: the
	// summary origin, the session id, and the run id read from the context.
	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID(sessionID, "run-repair-summary"),
		withRepairOrigins(originSlice(msgs, originSummary)),
	)

	line := repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(sessionID))
	require.NotNil(t, line)
	require.Equal(t, string(originSummary), line["origin"],
		"a repair made while summarizing must be localized as a summary, not persisted")
	require.Equal(t, "run-repair-summary", line["run_id"])
}

// TestRepairDiag_CountersIncrementPerCause proves the repair counters are
// cumulative and split by cause: two preparePrompt passes over histories with
// one inject and one drop each advance their cause's counter, and the two
// causes never share a counter.
//
// The counters are process totals (each repair line carries its cause's
// running total), so they are shared with every other parallel test in the
// package. This test therefore asserts the race-safe properties - the counter
// advanced past its pre-test value and the line's total includes this test's
// repair - rather than an exact delta, which a concurrent repair would break.
func TestRepairDiag_CountersIncrementPerCause(t *testing.T) {
	t.Parallel()
	// Snapshot the process-wide counters up front.
	injectedBefore := orphanRepairInjected.Load()
	droppedBefore := orphanRepairDropped.Load()

	env := testEnv(t)
	logs := captureJSONLogs(t)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)
	ctx := t.Context()

	// Pass 1: an orphaned call (inject).
	injSess, _ := seedOrphanedCallHistory(t, env)
	injMsgs, err := env.messages.List(ctx, injSess)
	require.NoError(t, err)
	_, _ = agent.preparePrompt(injMsgs, true, nil, nil,
		withRepairSessionID(injSess, "run-count-1"),
		withRepairOrigins(originSlice(injMsgs, originPersisted)),
	)
	injLine := repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(injSess))
	require.NotNil(t, injLine)
	injTotal, ok := injLine["orphan_repair_injected_total"].(float64)
	require.True(t, ok, "the inject line must carry the running injected-repair counter")
	// The line's total includes this test's one inject, so it is at least
	// pre-test + 1 (a concurrent repair only raises it, never lowers it).
	require.GreaterOrEqual(t, injTotal, float64(injectedBefore+1),
		"the injected counter total on the line must include this test's inject")

	// Pass 2: an orphaned result (drop).
	dropSess, _ := seedOrphanedResultHistory(t, env)
	dropMsgs, err := env.messages.List(ctx, dropSess)
	require.NoError(t, err)
	_, _ = agent.preparePrompt(dropMsgs, true, nil, nil,
		withRepairSessionID(dropSess, "run-count-2"),
		withRepairOrigins(originSlice(dropMsgs, originPersisted)),
	)
	dropLine := repairLine(t, logs, "Dropping orphaned tool result with no matching tool call", matchSessionID(dropSess))
	require.NotNil(t, dropLine)
	dropTotal, ok := dropLine["orphan_repair_dropped_total"].(float64)
	require.True(t, ok, "the drop line must carry the running dropped-repair counter")
	require.GreaterOrEqual(t, dropTotal, float64(droppedBefore+1),
		"the dropped counter total on the line must include this test's drop")

	// The two causes must not share a counter: the inject line carries only the
	// injected total, the drop line only the dropped total. This is the
	// "по причинам" (by cause) split.
	_, hasDropOnInject := injLine["orphan_repair_dropped_total"]
	require.False(t, hasDropOnInject, "an inject repair must not carry the dropped counter")
	_, hasInjOnDrop := dropLine["orphan_repair_injected_total"]
	require.False(t, hasInjOnDrop, "a drop repair must not carry the injected counter")

	// Both counters advanced past their pre-test values (this test's repairs
	// landed; concurrent repairs only push them higher).
	require.GreaterOrEqual(t, orphanRepairInjected.Load(), injectedBefore+1)
	require.GreaterOrEqual(t, orphanRepairDropped.Load(), droppedBefore+1)
}

// TestRepairDiag_NoSecretsInAnyLine is the blanket no-secrets assertion across
// both repair kinds at once: a history that mixes an orphaned call (with a
// secret input) and an orphaned result (with a secret body) produces two repair
// lines, and neither the buffer nor either line contains any of the secrets.
func TestRepairDiag_NoSecretsInAnyLine(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	ctx := t.Context()
	sess, err := env.sessions.Create(ctx, "repair-no-secrets")
	require.NoError(t, err)
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "start"}},
	})
	require.NoError(t, err)
	// Assistant with an orphaned call (secret input) and a result-bearing call.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ToolCall{ID: "call_a", Name: "bash", Input: `{"command":"echo SECRET_A"}`, Finished: true},
			message.ToolCall{ID: "call_b", Name: "read", Input: `{"path":"/x"}`, Finished: true},
		},
	})
	require.NoError(t, err)
	// Only call_b has a result; call_a is the orphan (inject). call_b's result
	// is present, so no drop here - the drop is added by the next tool message
	// referencing a call that was never made.
	_, err = env.messages.Create(ctx, sess.ID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: "call_b", Name: "read", Content: "ok"},
			message.ToolResult{ToolCallID: "call_c", Name: "grep", Content: "SECRET_C_BODY"},
		},
	})
	require.NoError(t, err)

	msgs, err := env.messages.List(ctx, sess.ID)
	require.NoError(t, err)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)
	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID(sess.ID, "run-no-secrets"),
		withRepairOrigins(originSlice(msgs, originPersisted)),
	)

	raw := logs.String()
	for _, secret := range []string{"SECRET_A", "SECRET_C_BODY"} {
		require.NotContains(t, raw, secret, "no repair line may log tool input or result content")
	}
	// Both repairs fired.
	require.NotNil(t, repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(sess.ID)))
	require.NotNil(t, repairLine(t, logs, "Dropping orphaned tool result with no matching tool call", matchSessionID(sess.ID)))
}

// TestRepairDiag_DefaultsAreSafe proves a preparePrompt call with no options
// (the T1 trim integration path, and every existing caller) still repairs and
// logs a line whose origin defaults to persisted and whose correlation ids are
// empty - the safe zero-value behavior. This is what keeps "T1 trim
// integration не должен repair" true: trimming must not *create* orphans, and
// when a pre-existing orphan is met with no options, the line is still emitted
// (the diagnostic) but tagged with the default persisted origin.
func TestRepairDiag_DefaultsAreSafe(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)

	sessionID, assistantMsgID := seedOrphanedCallHistory(t, env)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)
	ctx := t.Context()
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	// No options at all.
	_, _ = agent.preparePrompt(msgs, true, nil, nil)

	line := repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchMessageID(assistantMsgID))
	require.NotNil(t, line, "an orphan must still be repaired and logged with no options")
	require.Equal(t, string(originPersisted), line["origin"], "the default origin is persisted")
	require.Equal(t, "", line["session_id"], "no options means no correlation id is supplied")
	require.Equal(t, "", line["run_id"], "no options means no run id is supplied")
}

// TestRepairDiag_SuppressedEstimationPass proves the turn's token-estimation
// pass (withRepairSuppressed) still repairs the history into a valid prompt but
// emits no diagnostic line and bumps no counter. This is what stops the same
// orphan from being double-counted when preparePrompt runs a second time just to
// estimate tokens - the prompt the model sees is repaired and logged exactly
// once, by the main history pass.
func TestRepairDiag_SuppressedEstimationPass(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	sessionID, _ := seedOrphanedCallHistory(t, env)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)
	ctx := t.Context()
	msgs, err := env.messages.List(ctx, sessionID)
	require.NoError(t, err)

	// The suppressed (estimation) pass: repairs, but must not log or count.
	history, _ := agent.preparePrompt(msgs, true, nil, nil, withRepairSuppressed())
	// The repair still happened: the orphaned call got a synthetic result, so
	// the history is a valid prompt.
	var repaired bool
	for _, msg := range history {
		if msg.Role != fantasy.MessageRoleTool {
			continue
		}
		for _, part := range msg.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && tr.ToolCallID == "call_orphan" {
				repaired = true
			}
		}
	}
	require.True(t, repaired, "the suppressed pass must still repair the orphan into a valid prompt")

	// No diagnostic line for this session. recordRepair logs and bumps its
	// cause's counter in the same function, so the absence of a line is itself
	// the proof this pass did not bump the counter (the global counter is
	// shared with parallel tests, so it cannot be asserted as "unchanged" -
	// only "this pass contributed no line, hence no bump").
	require.Nil(t, repairLine(t, logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID(sessionID)),
		"a suppressed estimation pass must not emit a repair line")
	require.Nil(t, repairLine(t, logs, "Dropping orphaned tool result with no matching tool call", matchSessionID(sessionID)),
		"a suppressed estimation pass must not emit a drop line")
}

// countRepairLines counts the captured JSON repair lines for a msg that match
// the predicate. It is the "exactly one repair" primitive: repairLine finds the
// first, countRepairLines proves how many there are.
func countRepairLines(buf *syncLogBuffer, msg string, match func(map[string]any) bool) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			continue
		}
		if decoded["msg"] == msg && match(decoded) {
			n++
		}
	}
	return n
}

// repairLineByToolCallID scopes a repair line to the tool_call_id it repaired,
// which is unique per orphaned exchange even when the messages share (or lack)
// an id.
func repairLineByToolCallID(t *testing.T, buf *syncLogBuffer, msg, toolCallID string) map[string]any {
	t.Helper()
	return repairLine(t, buf, msg, func(line map[string]any) bool { return line["tool_call_id"] == toolCallID })
}

// assistantWithOrphanCall builds a direct (store-independent) assistant message
// with one orphaned tool call. Constructing message.Message by hand - rather
// than through the store - is what lets the positional-provenance tests
// duplicate or empty the message id, which the store (always a fresh UUID)
// cannot produce.
func assistantWithOrphanCall(id, toolCallID, name string) message.Message {
	return message.Message{
		ID:   id,
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.TextContent{Text: "working"},
			message.ToolCall{ID: toolCallID, Name: name, Input: `{}`, Finished: true},
		},
	}
}

// toolWithOrphanResult builds a direct tool message with one orphaned result
// (a result whose tool_call_id appears nowhere).
func toolWithOrphanResult(id, toolCallID, name string) message.Message {
	return message.Message{
		ID:   id,
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{ToolCallID: toolCallID, Name: name, Content: "ok"},
		},
	}
}

// TestRepairDiag_DuplicateIDsPositionalOrigin proves the origin is carried by
// position, not looked up by message id: two assistant messages that share the
// SAME non-empty id but sit at different positions with different origins each
// get the origin of their own position. A map keyed by id cannot represent this
// (the id would collapse to one entry, last-write-wins), so this test would fail
// under the old per-id provenance - it only holds because origins are aligned
// to the input slice by index.
func TestRepairDiag_DuplicateIDsPositionalOrigin(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	// Both messages carry the id "dup" (a non-empty duplicate), but they are at
	// different positions with different origins: index 0 carried, index 1
	// persisted. Their tool_call_ids are distinct so the lines are separable.
	msgs := []message.Message{
		assistantWithOrphanCall("dup", "call_dup_carried", "bash"),
		assistantWithOrphanCall("dup", "call_dup_persisted", "read"),
	}
	// Positional origins aligned to msgs: [carried, persisted].
	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID("sess-dup", "run-dup"),
		withRepairOrigins([]historyOrigin{originCarried, originPersisted}),
	)

	carried := repairLineByToolCallID(t, logs, "Injecting synthetic tool result for orphaned tool call", "call_dup_carried")
	require.NotNil(t, carried, "the carried-side orphaned call must be repaired")
	require.Equal(t, "dup", carried["message_id"],
		"the message id is logged as-is, even when duplicated")
	require.Equal(t, string(originCarried), carried["origin"],
		"the first message's origin must be carried, not the id's last-written value")

	persisted := repairLineByToolCallID(t, logs, "Injecting synthetic tool result for orphaned tool call", "call_dup_persisted")
	require.NotNil(t, persisted, "the persisted-side orphaned call must be repaired")
	require.Equal(t, "dup", persisted["message_id"],
		"the same message id is logged on the second line too")
	require.Equal(t, string(originPersisted), persisted["origin"],
		"the second message's origin must be persisted, independent of the first")
}

// TestRepairDiag_EmptyIDMixedOrigins proves the origin is independent of the
// message id when the id is empty: a history mixing an empty-id message and a
// non-empty-id message, at different positions with different origins, gets
// each line's origin from position - the empty-id message is not defaulted or
// conflated with the other. A map keyed by id cannot key an empty string to two
// different messages, so this only holds with positional provenance.
func TestRepairDiag_EmptyIDMixedOrigins(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	// Index 0 has an EMPTY id (carried), index 1 a non-empty id (persisted).
	// Both are orphaned. The empty-id one must still carry carried.
	msgs := []message.Message{
		assistantWithOrphanCall("", "call_empty_carried", "bash"),
		assistantWithOrphanCall("real-id", "call_named_persisted", "read"),
	}
	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID("sess-empty", "run-empty"),
		withRepairOrigins([]historyOrigin{originCarried, originPersisted}),
	)

	emptyLine := repairLineByToolCallID(t, logs, "Injecting synthetic tool result for orphaned tool call", "call_empty_carried")
	require.NotNil(t, emptyLine, "the empty-id orphaned call must be repaired and logged")
	require.Equal(t, "", emptyLine["message_id"],
		"an empty message id is logged as empty, not defaulted")
	require.Equal(t, string(originCarried), emptyLine["origin"],
		"the empty-id message's origin comes from its position, not a failed id lookup")

	namedLine := repairLineByToolCallID(t, logs, "Injecting synthetic tool result for orphaned tool call", "call_named_persisted")
	require.NotNil(t, namedLine)
	require.Equal(t, "real-id", namedLine["message_id"])
	require.Equal(t, string(originPersisted), namedLine["origin"],
		"the named message keeps its own positional origin")
}

// TestRepairDiag_MultipleEmptyIDsOneRepair proves the "multiple empties, only
// one repair" case: a history of several empty-id messages where only ONE is
// orphaned produces exactly one repair line, and that line carries the origin
// of the position that is orphaned - not the origin of the non-orphaned
// empty-id siblings. This is the failure mode the per-id map was prone to:
// several ""-keyed entries collapse, and the one repair's origin would be
// ambiguous. Positionally, the orphan's own index decides.
func TestRepairDiag_MultipleEmptyIDsOneRepair(t *testing.T) {
	t.Parallel()
	env := testEnv(t)
	logs := captureJSONLogs(t)
	sa := testSessionAgent(env, nil, "system")
	agent := sa.(*sessionAgent)

	// Three empty-id messages. Only the middle one (index 1) has an orphaned
	// call; the others have calls that are answered, so they produce no repair.
	// Origins differ per position: [persisted, carried, persisted].
	answered := message.ToolResult{ToolCallID: "call_answered_1", Name: "bash", Content: "ok"}
	answered2 := message.ToolResult{ToolCallID: "call_answered_2", Name: "read", Content: "ok"}
	msgs := []message.Message{
		message.Message{
			ID:   "",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "a"},
				message.ToolCall{ID: "call_answered_1", Name: "bash", Input: `{}`, Finished: true},
			},
		},
		// Index 1: the lone orphan (carried).
		assistantWithOrphanCall("", "call_lone_orphan", "glob"),
		message.Message{
			ID:   "",
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "c"},
				message.ToolCall{ID: "call_answered_2", Name: "read", Input: `{}`, Finished: true},
			},
		},
	}
	// The answered results live in tool messages (also empty-id) so the two
	// answered calls are not orphans; only call_lone_orphan is.
	msgs = append(msgs,
		message.Message{ID: "", Role: message.Tool, Parts: []message.ContentPart{answered, answered2}},
	)

	_, _ = agent.preparePrompt(msgs, true, nil, nil,
		withRepairSessionID("sess-multi", "run-multi"),
		withRepairOrigins([]historyOrigin{originPersisted, originCarried, originPersisted, originPersisted}),
	)

	// Exactly one repair line for this session, and it is the lone orphan's.
	require.Equal(t, 1, countRepairLines(logs, "Injecting synthetic tool result for orphaned tool call", matchSessionID("sess-multi")),
		"only the single orphaned call must be repaired, the answered ones must not")
	line := repairLineByToolCallID(t, logs, "Injecting synthetic tool result for orphaned tool call", "call_lone_orphan")
	require.NotNil(t, line)
	require.Equal(t, "", line["message_id"], "the orphaned message has an empty id")
	require.Equal(t, string(originCarried), line["origin"],
		"the one repair must carry the origin of the orphan's own position (index 1 = carried), not a sibling's")
}
