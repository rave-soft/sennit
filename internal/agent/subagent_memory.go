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
// is always kept even when it alone exceeds the budget: returning
// nothing there would silently turn the most recent exchange - the one
// the next delegation almost certainly follows on from - into the one
// piece of context that goes missing.
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
	return carried, first
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
