package agent

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

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

// trimToBudget keeps the largest suffix that fits budget without separating an
// assistant tool call from any result carrying its ID. Exchange membership is
// determined by IDs, rather than adjacent roles: assistant text and reasoning
// may appear between a call and its result.
//
// If an exchange itself cannot fit, it is omitted and represented by a single,
// budget-aware text placeholder. The placeholder has no tool parts, so it
// cannot create protocol repairs when the carried history reaches preparePrompt.
func trimToBudget(msgs []message.Message, budget int) []message.Message {
	result := trimToBudgetResult(msgs, budget)
	if result.trimmed {
		logTrim(result, budget)
	}
	return result.messages
}

type trimResult struct {
	messages                               []message.Message
	kept, dropped, truncated, placeholders int
	trimmed                                bool
}

func trimToBudgetResult(msgs []message.Message, budget int) trimResult {
	if budget <= 0 || len(msgs) == 0 {
		return trimResult{trimmed: len(msgs) > 0, dropped: len(msgs)}
	}
	if messagesTextLen(msgs) <= budget {
		return trimResult{messages: msgs, kept: len(msgs)}
	}

	exchanges := findToolExchanges(msgs)
	choice := bestSuffixCut(msgs, exchanges, 0, budget)
	if choice >= 0 {
		result := trimResult{
			messages: msgs[choice:],
			kept:     len(msgs) - choice,
			dropped:  choice,
			trimmed:  true,
		}
		// If the immediately preceding complete exchange was too large, leave
		// a compact record of it while preserving the newest tail first.
		if exchange, ok := exchangeEndingAt(exchanges, choice); ok && messagesTextLen(msgs[exchange.start:exchange.end]) > budget {
			if stub := oversizedExchangePlaceholder(exchange.calls, budget-messagesTextLen(result.messages)); len(stub) > 0 {
				result.messages = append(stub, result.messages...)
				result.placeholders = len(stub)
			}
		}
		return result
	}

	// No whole-message suffix fits. A textual tail is still useful context;
	// retain as much of its final plain-text message as the budget permits.
	if tail := truncatedPlainTextTail(msgs[len(msgs)-1], budget); len(tail) > 0 {
		return trimResult{
			messages:  tail,
			dropped:   len(msgs) - 1,
			truncated: 1,
			trimmed:   true,
		}
	}

	// An oversized final exchange has no following message boundary. Replace it
	// with the deterministic text stub, which is itself clipped to budget.
	if exchange, ok := exchangeContaining(exchanges, len(msgs)-1); ok && exchange.end == len(msgs) {
		result := oversizedExchangePlaceholder(exchange.calls, budget)
		return trimResult{
			messages:     result,
			dropped:      len(msgs),
			placeholders: len(result),
			trimmed:      true,
		}
	}
	return trimResult{dropped: len(msgs), trimmed: true}
}

type toolExchange struct {
	start, end int // end is exclusive
	calls      []message.ToolCall
}

// findToolExchanges associates results with calls by ToolCallID. It does not
// assume that tool messages are adjacent to the declaring assistant message.
func findToolExchanges(msgs []message.Message) []toolExchange {
	callAt := make(map[string]int)
	callsAt := make(map[int][]message.ToolCall)
	for i, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		for _, call := range m.ToolCalls() {
			if call.ID == "" {
				continue
			}
			callAt[call.ID] = i
			callsAt[i] = append(callsAt[i], call)
		}
	}
	ends := make(map[int]int)
	for start := range callsAt {
		ends[start] = start + 1
	}
	for i, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, result := range m.ToolResults() {
			if start, ok := callAt[result.ToolCallID]; ok && i+1 > ends[start] {
				ends[start] = i + 1
			}
		}
	}
	exchanges := make([]toolExchange, 0, len(callsAt))
	for start, calls := range callsAt {
		exchanges = append(exchanges, toolExchange{start: start, end: ends[start], calls: calls})
	}
	slices.SortFunc(exchanges, func(a, b toolExchange) int { return a.start - b.start })
	return exchanges
}

// bestSuffixCut considers every message boundary. A boundary is rejected only
// when its suffix contains an orphaned result.
func bestSuffixCut(msgs []message.Message, _ []toolExchange, minimum, budget int) int {
	for cut := minimum; cut < len(msgs); cut++ {
		if messagesTextLen(msgs[cut:]) > budget || hasOrphanResult(msgs[cut:]) {
			continue
		}
		return cut
	}
	return -1
}

func exchangeEndingAt(exchanges []toolExchange, end int) (toolExchange, bool) {
	for _, exchange := range exchanges {
		if exchange.end == end {
			return exchange, true
		}
	}
	return toolExchange{}, false
}

func exchangeContaining(exchanges []toolExchange, index int) (toolExchange, bool) {
	for _, exchange := range exchanges {
		if exchange.start <= index && index < exchange.end {
			return exchange, true
		}
	}
	return toolExchange{}, false
}

func hasOrphanResult(msgs []message.Message) bool {
	known := make(map[string]struct{})
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		for _, call := range m.ToolCalls() {
			known[call.ID] = struct{}{}
		}
	}
	for _, m := range msgs {
		if m.Role != message.Tool {
			continue
		}
		for _, result := range m.ToolResults() {
			if _, ok := known[result.ToolCallID]; !ok {
				return true
			}
		}
	}
	return false
}

// oversizedExchangePlaceholder returns at most one plain text message. Keeping
// it as text only is important: conversion and persistence must never see a
// synthetic ToolCall for an omitted exchange.
func oversizedExchangePlaceholder(calls []message.ToolCall, budget int) []message.Message {
	if budget <= 0 {
		return nil
	}
	const prefix = "Earlier tool exchange omitted: "
	var names []string
	for _, call := range calls {
		name := call.Name
		if name == "" {
			name = "tool"
		}
		names = append(names, name)
	}
	text := prefix + strings.Join(names, ", ")
	text = truncateUTF8(text, budget)
	return []message.Message{{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}}}
}

func truncatedPlainTextTail(m message.Message, budget int) []message.Message {
	if budget <= 0 || len(m.Parts) != 1 {
		return nil
	}
	text, ok := m.Parts[0].(message.TextContent)
	if !ok || text.Text == "" {
		return nil
	}
	text.Text = suffixUTF8(text.Text, budget)
	return []message.Message{{Role: m.Role, Parts: []message.ContentPart{text}}}
}

// logTrim reports source-message accounting: dropped_messages counts original
// messages excluded completely; kept_messages counts original messages retained
// unchanged. Replacements are reported separately as truncated_messages and
// placeholder_messages.
func logTrim(result trimResult, budget int) {
	slog.Info("Trimmed the carried sub-agent session to the budget",
		"dropped_messages", result.dropped,
		"kept_messages", result.kept,
		"truncated_messages", result.truncated,
		"placeholder_messages", result.placeholders,
		"kept_chars", messagesTextLen(result.messages),
		"budget", budget,
	)
}

// truncateUTF8 returns the longest valid UTF-8 prefix within the byte budget.
func truncateUTF8(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(text) <= budget {
		return text
	}
	end := budget
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// suffixUTF8 returns the longest valid UTF-8 suffix within the byte budget.
func suffixUTF8(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(text) <= budget {
		return text
	}
	start := len(text) - budget
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
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
