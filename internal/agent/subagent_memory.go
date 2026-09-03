package agent

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"

	"github.com/rave-soft/sennit/internal/message"
)

// maxCarriedSubAgentChars is the hard cap on how many UTF-8 bytes of a named
// sub-agent's earlier conversation are replayed into its next delegation.
// Existing trimming APIs use byte lengths, so the historical name remains for
// compatibility even though the unit is now explicitly bytes.
//
// Without a provider tokenizer, every UTF-8 byte is charged as one possible
// token. This deliberately overestimates ordinary prose but remains safe for
// dense code, JSON, punctuation and multilingual text where a four-character
// average is not conservative. The cap remains the final guard rail for large
// or unknown context windows.
const maxCarriedSubAgentChars = 120_000

// carryOverSafetyMargin is a flat token reserve. Because the entire budget is
// evaluated at one UTF-8 byte per possible token, its numeric value is also
// the number of bytes withheld from carried history. It covers provider-side
// message framing and other request overhead not represented in our strings.
const carryOverSafetyMargin = 8_000

// carryOverBudgetInput carries everything carryOverBudget needs to size a
// named sub-agent's carried-history budget from the model that will run the
// delegation and the concrete runtime that will frame its first call.
//
// The point of carrying the actual byte sizes here - rather than working
// from fixed upper bounds - is that the budget has to hold for the *real*
// system prompt, tool schemas and delegation prompt this call is about to
// send, not for a worst case we guessed. A coder prompt with the built-in
// tool set is tens of kilobytes; an MCP-heavy agent can be larger still, and
// a fixed reserve would either starve the carried history (too large) or let
// the total past the window (too small) depending on which we chose.
type carryOverBudgetInput struct {
	// Model is the model the delegation runs on: its context window and
	// output capacity.
	Model Model
	// SystemPromptBytes is the length in bytes of the actual system prompt
	// the delegate will send (after the agent's build has landed it).
	SystemPromptBytes int
	// ToolSchemaBytes is the total length in bytes of the serialized tool
	// schemas the delegate will send.
	ToolSchemaBytes int
	// PromptBytes is the length in bytes of this delegation's own prompt.
	PromptBytes int
}

// carryOverBudget returns the UTF-8 byte budget for carried history. Context
// windows and output capacities are token counts, but no tokenizer is shared
// by every provider. We therefore use the conservative upper estimate of one
// token per UTF-8 byte: a token window of N permits at most N counted bytes.
// Fixed request data, history, output reserve and safety margin all use that
// same unit, so code, JSON and multi-byte Unicode cannot exploit a prose-based
// average to overfill the context.
//
// A model whose context window is unknown falls back to the historical hard
// cap because no finite relationship to its real window can be inferred.
func carryOverBudget(in carryOverBudgetInput) int {
	contextTokens := in.Model.CatalogCfg.ContextWindow
	if contextTokens <= 0 {
		return maxCarriedSubAgentChars
	}

	fixedBytes := saturatingAdd(
		saturatingAdd(nonNegativeIntToInt64(in.SystemPromptBytes), nonNegativeIntToInt64(in.ToolSchemaBytes)),
		nonNegativeIntToInt64(in.PromptBytes),
	)
	reserves := saturatingAdd(
		saturatingAdd(fixedBytes, outputCapacityTokens(in.Model)),
		carryOverSafetyMargin,
	)
	budget := saturatingSub(contextTokens, reserves)
	if budget > maxCarriedSubAgentChars {
		budget = maxCarriedSubAgentChars
	}
	if budget > maxInt64ForInt {
		return maxInt
	}
	return int(budget)
}

// maxInt is the largest int, used to clamp the final budget without a
// platform-specific constant. It is int-typed so the return is the right
// width on every build.
const maxInt = int(^uint(0) >> 1)

// maxInt64ForInt is the largest value representable as int, expressed as
// int64 so it can be compared against the int64 budget before the final
// conversion. On every supported build (int is at least 32 bits) this is
// well below MaxInt64, so the comparison is always in range.
const maxInt64ForInt = int64(^uint(0) >> 1)

func saturatingAdd(a, b int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if a <= 0 {
		return max(0, b)
	}
	if b <= 0 {
		return a
	}
	if a > maxInt64-b {
		return maxInt64
	}
	return a + b
}

func saturatingSub(a, b int64) int64 {
	if a <= b {
		return 0
	}
	return a - b
}

func nonNegativeIntToInt64(v int) int64 {
	if v <= 0 {
		return 0
	}
	return int64(v)
}

// outputCapacityTokens is the maximum number of tokens a model's reply may
// be, read for the purpose of *reserving room in a context window* - not for
// the purpose of setting the max_output_tokens transport field.
//
// The two questions are different, and confusing them is what this function
// exists to prevent. modelMaxOutputTokens answers "what cap do we send on the
// wire", and it deliberately reports zero for providers that reject the field
// (Codex answers it with a 400). The budget has to reserve room for a reply
// the model *can* write even on those providers, so it must not inherit that
// suppression: a Codex delegation still writes a reply, and the window has to
// hold it.
//
// The precedence is the explicit per-model setting, then the catalog default.
// If both are unknown, half of the known context window is reserved. That
// intentionally sacrifices continuity rather than assuming a small 4k reply:
// an uncapped model may legally produce much more, and history must not compete
// with that unknown output. Reserving half still permits useful history while
// ensuring fixed input and history together cannot claim the whole window.
// The policy is provider-independent and is used only for budgeting; it does
// not invent a max_output_tokens transport value.
func outputCapacityTokens(model Model) int64 {
	if model.ModelCfg.MaxTokens > 0 {
		return model.ModelCfg.MaxTokens
	}
	if model.CatalogCfg.DefaultMaxTokens > 0 {
		return model.CatalogCfg.DefaultMaxTokens
	}
	if model.CatalogCfg.ContextWindow > 0 {
		return model.CatalogCfg.ContextWindow / 2
	}
	return 0
}

// toolSchemaBytes is the total length in bytes of the serialized tool
// schemas a set of tools will occupy on the wire: each tool's ToolInfo
// marshalled to JSON, summed. This is the same shape the provider receives
// in the tools field of the request, so it is the honest measure of what the
// schemas cost the window - not a fixed reserve.
//
// A tool whose Info fails to marshal is counted at zero rather than
// failing the budget: the budget is a guard rail, and a single malformed
// schema is better over-counted (carrying a little less history) than
// allowed to break the whole delegation.
func toolSchemaBytes(tools []fantasy.AgentTool) int {
	var total int64
	for _, tool := range tools {
		b, err := json.Marshal(preparedFunctionTool(tool))
		if err != nil {
			continue
		}
		total = saturatingAdd(total, int64(len(b)))
	}
	if total > maxInt64ForInt {
		return maxInt
	}
	return int(total)
}

// preparedFunctionTool mirrors fantasy's Agent.prepareTools mapping. Fantasy
// does not expose that mapper, so keeping this small, tested conversion here
// makes the budget count the same FunctionTool envelope and schema object that
// reaches providers.
func preparedFunctionTool(tool fantasy.AgentTool) fantasy.FunctionTool {
	info := tool.Info()
	inputSchema := info.InputSchema
	if inputSchema == nil {
		inputSchema = map[string]any{
			"type":       "object",
			"properties": info.Parameters,
			"required":   info.Required,
		}
		schema.Normalize(inputSchema)
	}
	// ToolInfo values can be backed by mutable MCP SDK maps; duplicate before
	// marshalling the budget envelope so this path cannot mutate published data.
	inputSchema = cloneJSONSchema(inputSchema)
	return fantasy.FunctionTool{
		Name:            info.Name,
		Description:     info.Description,
		InputSchema:     inputSchema,
		ProviderOptions: tool.ProviderOptions(),
	}
}

func cloneJSONSchema(input map[string]any) map[string]any {
	data, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	var cloned map[string]any
	if json.Unmarshal(data, &cloned) != nil {
		return nil
	}
	return cloned
}

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
// a delegation, while failing it outright would break a delegation that
// would otherwise succeed.
//
// in carries the model and the concrete runtime byte sizes used to size the
// carry-over budget (see carryOverBudget). The runtime byte sizes are read
// from the delegate *after* its build has landed (the caller waits on
// waitReady first), so the budget is computed from the actual system prompt,
// tool schemas and delegation prompt this call will send - not from
// conservative upper bounds.
// trimCorrelation carries the session/run ids a trim log line echoes (T5), so
// the carried-history trim is correlatable by the same ids the provider and
// repair lines use. It is variadic on trimToBudget/applyCarryOverBudget so the
// many test callers (which pass no correlation) keep their current 2-arg form;
// the production call site (carryOverMessages) passes the ids.
type trimCorrelation struct {
	sessionID string
	runID     string
}

func trimCorr(sessionID, runID string) trimCorrelation {
	return trimCorrelation{sessionID: sessionID, runID: runID}
}

func applyCarryOverBudget(perSession [][]message.Message, budget int, corr ...trimCorrelation) ([]message.Message, int) {
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
		carried = trimToBudget(carried, budget, corr...)
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
func trimToBudget(msgs []message.Message, budget int, corr ...trimCorrelation) []message.Message {
	result := trimToBudgetResult(msgs, budget)
	if result.trimmed {
		var c trimCorrelation
		if len(corr) > 0 {
			c = corr[0]
		}
		logTrim(result, budget, c)
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
// placeholder_messages. It also echoes session_id/run_id (T5) so the carried-
// history trim is correlatable by the same ids the provider and repair lines
// use; the ids are empty when the caller (a test, or a path with no run id)
// did not supply them.
func logTrim(result trimResult, budget int, c trimCorrelation) {
	slog.Info("Trimmed the carried sub-agent session to the budget",
		"session_id", c.sessionID,
		"run_id", c.runID,
		"dropped_messages", result.dropped,
		"kept_messages", result.kept,
		"truncated_messages", result.truncated,
		"placeholder_messages", result.placeholders,
		"kept_bytes", messagesTextLen(result.messages),
		"budget_bytes", budget,
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
