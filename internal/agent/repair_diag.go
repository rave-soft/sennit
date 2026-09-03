package agent

import (
	"log/slog"
	"sync/atomic"

	"github.com/rave-soft/sennit/internal/message"
)

// historyOrigin is where a message's history came from, carried into
// preparePrompt for orphan-repair diagnostics (T4). It is the *source* of the
// history being prepared, so a repair log line can say which side of the
// history an orphan came from without cross-referencing other events:
//
//   - persisted: the session's own persisted history (its own messages).
//   - carried:   history carried in from another session (a named sub-agent's
//     earlier sessions, via SessionAgentCall.PriorMessages).
//   - summary:   the pass a summarize is compressing (usage.go's summarize).
//
// It is carried positionally - one origin per input message, aligned by index
// to the slice preparePrompt iterates - rather than keyed by message id. The
// turn path prepends carried messages in front of the session's own, and
// message ids are not a safe join key for this: two messages may share an id,
// an id may be empty, and preparePrompt's own filtering/conversion is not
// guaranteed to preserve id-to-message one-to-one. Index alignment to the
// input slice is stable under all of those, so that is what we carry.
type historyOrigin string

const (
	originPersisted historyOrigin = "persisted"
	originCarried   historyOrigin = "carried"
	originSummary   historyOrigin = "summary"
)

// repair actions recorded on the "orphan repair" diagnostic line (T4). They
// are the two distinct fixes preparePrompt applies to a broken tool exchange:
//
//   - repairDropResult: a tool *result* had no matching tool *call*, so the
//     result part was dropped. The call's message was removed (a prior
//     summary, a trim, or a cancel that dropped the assistant message but kept
//     the tool message); the result is stale.
//   - repairInjectResult: a tool *call* had no matching tool *result*, so a
//     synthetic error result was injected. This is the interrupted-stream
//     signature: the model emitted the call, but the stream was cut (cancel,
//     error, or an interrupted turn) before the result was persisted.
const (
	repairDropResult   = "drop_result"
	repairInjectResult = "inject_result"
)

// orphanRepairDropped / orphanRepairInjected are the running counters of
// repairs by cause. They are process totals, not per-session: each repair
// line logs its cause's running total so
// a single line carries the cumulative count for that cause without a second
// aggregation pass. They are atomics because preparePrompt runs on many
// session goroutines at once.
var (
	orphanRepairDropped  atomic.Int64
	orphanRepairInjected atomic.Int64
)

// preparePromptOptions is the typed carrier for the orphan-repair diagnostics
// preparePrompt needs (T4). It is passed via preparePromptOption so the many
// existing callers (and the T1 trim path, which must NOT repair) keep their
// current signature and behavior. A zero value means "no correlation, every
// message is persisted-origin, logging enabled" - the safe default for callers
// that do not care about repair diagnostics.
type preparePromptOptions struct {
	// sessionID and runID are the correlation ids echoed on every repair line
	// so an orphan can be localized to one session/turn by a single log entry.
	sessionID string
	runID     string
	// origins is the positional history origin per input message: origins[i]
	// is the origin of msgs[i], aligned by index to the slice preparePrompt
	// iterates. It is NOT keyed by message id (ids may be empty or duplicated
	// and are not preserved one-to-one through the conversion). A nil or
	// short slice defaults every message to originPersisted.
	origins []historyOrigin
	// suppressed, when true, makes the repair sites still repair (the prompt
	// must be valid) but skip the diagnostic log line and the counter bump. The
	// turn's token-estimation pass uses it: it repairs the same history a
	// second time just to count tokens, and that second repair is not the
	// prompt the model sees, so logging it would double-count the same orphan
	// and add a misleading line.
	suppressed bool
}

// preparePromptOption mutates a preparePromptOptions. The functional-options
// form keeps preparePrompt's signature stable (existing callers pass nothing)
// while letting a caller attach just the fields it knows.
type preparePromptOption func(*preparePromptOptions)

// withRepairSessionID tags every repair preparePrompt emits with the session
// and run ids, so the orphan can be localized by one log line. Empty values
// are logged as empty strings (the caller had no correlation to supply).
func withRepairSessionID(sessionID, runID string) preparePromptOption {
	return func(o *preparePromptOptions) {
		o.sessionID = sessionID
		o.runID = runID
	}
}

// withRepairOrigins carries the positional history origins into preparePrompt.
// origins must be aligned by index to the msgs slice this call is preparing:
// origins[i] is the origin of msgs[i]. A caller that mixes sources (the turn
// path prepends carried messages in front of the session's own) passes a slice
// as long as the combined slice, with the carried half tagged carried and the
// own half tagged persisted. A nil/short origins defaults every message to
// originPersisted.
func withRepairOrigins(origins []historyOrigin) preparePromptOption {
	return func(o *preparePromptOptions) {
		o.origins = origins
	}
}

// withRepairSuppressed makes the repair sites still repair (the prompt must
// stay valid) but skip the diagnostic log line and the counter bump. The turn's
// token-estimation pass uses it: it runs preparePrompt a second time over the
// session's own history just to count tokens, and that second repair is not the
// prompt the model sees - logging it would double-count the same orphan and add
// a line that does not correspond to a request actually sent.
func withRepairSuppressed() preparePromptOption {
	return func(o *preparePromptOptions) {
		o.suppressed = true
	}
}

// originAt returns the positional origin of input message i, defaulting to
// originPersisted when origins is nil or shorter than i+1. This keeps the
// default safe: a message whose origin the caller did not carry is treated as
// the session's own persisted history.
func (o preparePromptOptions) originAt(i int) historyOrigin {
	if i >= 0 && i < len(o.origins) {
		return o.origins[i]
	}
	return originPersisted
}

// repairLogFields builds the structured fields every orphan-repair line
// carries (T4). It is the single place that decides the field names so the
// two repair sites (drop/inject) stay consistent and a future T5 sennit_logs
// filter can match them. It never includes message content or tool input:
// only ids, the source, the action, and the cumulative counters.
//
// origin is carried positionally by the caller (o.originAt(i) for the input
// message index i the repair belongs to) and is independent of messageID: the
// id is logged as-is when present, but it is never used to look the origin up.
//
// interrupted is the fourth source dimension (T4's "interrupted stream"): an
// orphaned *call* is by definition the interrupted-stream signature (the call
// was emitted but its result never arrived), so it is true for inject_result
// repairs and false for drop_result repairs (a stale result whose call was
// dropped). It is derived from the action, not guessed.
func repairLogFields(o preparePromptOptions, messageID string, origin historyOrigin, action, toolCallID, toolName string) []any {
	fields := []any{
		"session_id", o.sessionID,
		"run_id", o.runID,
		"message_id", messageID,
		"origin", origin,
		"repair_action", action,
		"tool_call_id", toolCallID,
		"tool_name", toolName,
	}
	if action == repairInjectResult {
		// An orphaned call is the interrupted-stream signature.
		fields = append(fields, "interrupted", true)
	} else {
		fields = append(fields, "interrupted", false)
	}
	return fields
}

// recordRepair logs the repair and bumps its cause's running counter,
// returning the cause-specific total to fold into the line. Keeping the bump
// and the log together in one function is what makes the counter and the line
// stay in lockstep (a repair that is logged is counted, and vice versa). A
// suppressed options (the turn's token-estimation pass) skips both: the
// prompt is still repaired by the caller, but the second, non-prompt repair is
// not logged or counted, so the same orphan is not double-reported. origin is
// the positionally-carried history origin for the message index the repair
// belongs to.
func recordRepair(msg string, o preparePromptOptions, messageID string, origin historyOrigin, action, toolCallID, toolName string) {
	if o.suppressed {
		return
	}
	fields := repairLogFields(o, messageID, origin, action, toolCallID, toolName)
	switch action {
	case repairInjectResult:
		fields = append(fields, "orphan_repair_injected_total", orphanRepairInjected.Add(1))
	default: // repairDropResult
		fields = append(fields, "orphan_repair_dropped_total", orphanRepairDropped.Add(1))
	}
	slog.Warn(msg, fields...)
}

// originSlice builds the positional origin slice for a single-source call:
// one origin per message, all the same origin, aligned by index to msgs.
func originSlice(msgs []message.Message, origin historyOrigin) []historyOrigin {
	if len(msgs) == 0 {
		return nil
	}
	origins := make([]historyOrigin, len(msgs))
	for i := range origins {
		origins[i] = origin
	}
	return origins
}

// turnOrigins builds the positional origin slice for the turn's combined
// history (the result of withPriorMessages(prior, own)): the carried-in
// messages (a named sub-agent's other sessions) are tagged carried, the
// session's own messages persisted. It is the "really carry the origin, don't
// guess it" path, done positionally: the turn prepends carried messages in
// front of its own, and the slice is aligned by index to the combined input,
// so the split is by position - not by message id, which may be empty or
// duplicated and is not a reliable join key.
func turnOrigins(prior, own []message.Message) []historyOrigin {
	total := len(prior) + len(own)
	if total == 0 {
		return nil
	}
	origins := make([]historyOrigin, total)
	for i := range prior {
		origins[i] = originCarried
	}
	for i := range own {
		origins[len(prior)+i] = originPersisted
	}
	return origins
}
