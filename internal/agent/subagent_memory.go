package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/rave-soft/sennit/internal/message"
)

// maxCarriedSubAgentChars bounds how much of a named sub-agent's earlier
// conversation is replayed into its next delegation.
//
// The budget is measured in characters of persisted message text rather
// than tokens on purpose: it is a guard rail, not an accounting figure,
// and it has to hold for every provider without a tokenizer in the loop.
// At roughly four characters per token this leaves the carried history
// around 30k tokens - enough for a working relationship across many
// delegations, small enough that a long-lived agent cannot quietly eat a
// context window that the current task still needs.
const maxCarriedSubAgentChars = 120_000

// carryOverMessages assembles the history a named sub-agent should see
// ahead of the delegation about to start: every message from its earlier
// sessions under the same parent, oldest first.
//
// This is what makes a named agent a continuing counterpart rather than
// a stranger on each call. Each delegation still gets its own session -
// the UI addresses a delegation block by session id, and collapsing them
// into one shared session would make every block show the same growing
// transcript - so continuity lives here, in what the model is shown,
// instead of in where the messages are stored.
//
// Scope follows from the parent session: delegations made inside a
// thread carry over only within that thread, because the thread's own
// session is the parent. Anonymous delegations (the built-in `agent` and
// `agentic_fetch` tools) pass an empty agentID and get nothing, which is
// deliberate - they are one-off, frequently parallel, and unrelated to
// each other.
//
// Errors are reported to the caller rather than swallowed, but the
// caller treats them as "no carried history": losing continuity degrades
// a delegation, failing it outright would break work that used to run.
func (c *coordinator) carryOverMessages(ctx context.Context, parentSessionID, agentID, currentSessionID string) ([]message.Message, error) {
	if agentID == "" {
		return nil, nil
	}

	prior, err := c.sessions.ListSubAgentSessions(ctx, parentSessionID, agentID, currentSessionID)
	if err != nil {
		return nil, fmt.Errorf("list prior %s sessions: %w", agentID, err)
	}
	if len(prior) == 0 {
		return nil, nil
	}

	// Per-session slices, kept apart until the budget has been applied:
	// the budget drops whole sessions, never half of an exchange.
	perSession := make([][]message.Message, 0, len(prior))
	for _, s := range prior {
		msgs, err := c.messages.List(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("list messages of prior %s session %s: %w", agentID, s.ID, err)
		}
		msgs = trimToSummary(s, msgs)
		if len(msgs) == 0 {
			continue
		}
		perSession = append(perSession, msgs)
	}

	kept, dropped := applyCarryOverBudget(perSession, maxCarriedSubAgentChars)
	if dropped > 0 {
		slog.Info(
			"Dropped oldest sub-agent sessions from carried history",
			"agent", agentID,
			"parent_session", parentSessionID,
			"dropped_sessions", dropped,
			"kept_sessions", len(perSession)-dropped,
		)
	}
	return kept, nil
}

// applyCarryOverBudget keeps the newest whole sessions that fit in
// budget and reports how many older ones it dropped. The newest session
// is always kept: returning nothing there would silently turn the most
// recent exchange - the one the next delegation almost certainly follows
// on from - into the one piece of context that goes missing.
//
// Kept, but no longer kept whole. A single delegation can run for
// hundreds of messages and carry megabytes of tool output, and replaying
// one of those verbatim put a quarter of a million tokens in front of a
// sub-agent whose own task had not started yet. Every turn then died on
// the provider's context limit, and auto-summarize could not save it -
// the session's own history was a rounding error next to the carried
// one, so there was nothing there to summarize (see
// runTurn.stopOnContextWindow). The budget has to bind here too, or it
// is not a budget.
func applyCarryOverBudget(perSession [][]message.Message, budget int) ([]message.Message, int) {
	if len(perSession) == 0 {
		return nil, 0
	}

	first := 0
	total := 0
	for i := len(perSession) - 1; i >= 0; i-- {
		size := messagesTextLen(perSession[i])
		if i < len(perSession)-1 && total+size > budget {
			first = i + 1
			break
		}
		total += size
	}

	var carried []message.Message
	for _, msgs := range perSession[first:] {
		carried = append(carried, msgs...)
	}
	if first == len(perSession)-1 {
		carried = trimToBudget(carried, budget)
	}
	return carried, first
}

// trimToBudget drops whole messages from the front of a single session's
// history until what is left fits in budget, keeping its tail: the end of
// a delegation - what was decided, what was handed back - is what the next
// one builds on, while the start is the exploration that got there.
//
// The last message is always kept, however big it is, for the same reason
// the newest session is: something of the previous exchange has to
// survive. Cutting mid-session can leave a tool result whose call is gone,
// or a call whose result is; preparePrompt already drops the one and
// answers the other (see compat.go), so the cut does not have to fall on
// an exchange boundary.
func trimToBudget(msgs []message.Message, budget int) []message.Message {
	if len(msgs) == 0 || messagesTextLen(msgs) <= budget {
		return msgs
	}

	first := len(msgs) - 1
	total := messagesTextLen(msgs[first:])
	for i := len(msgs) - 2; i >= 0; i-- {
		size := messagesTextLen(msgs[i : i+1])
		if total+size > budget {
			break
		}
		total += size
		first = i
	}
	slog.Info(
		"Trimmed the carried sub-agent session to the budget",
		"dropped_messages", first,
		"kept_messages", len(msgs)-first,
		"kept_chars", total,
		"budget", budget,
	)
	return msgs[first:]
}

// messagesTextLen sizes a session by the text its messages carry.
// Non-text parts (images, file attachments) are not counted: they are
// not what makes a transcript grow across delegations, and weighing them
// by their encoded size would make the budget swing on one screenshot.
func messagesTextLen(msgs []message.Message) int {
	n := 0
	for _, m := range msgs {
		for _, part := range m.Parts {
			switch p := part.(type) {
			case message.TextContent:
				n += len(p.Text)
			case message.ReasoningContent:
				n += len(p.Thinking)
			case message.ToolCall:
				n += len(p.Name) + len(p.Input)
			case message.ToolResult:
				n += len(p.Content)
			}
		}
	}
	return n
}
