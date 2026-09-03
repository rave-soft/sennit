package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/stringext"
)

// preparePrompt converts the session's persisted (and, for a named sub-agent,
// carried-in) history into the fantasy messages a turn sends to the model,
// repairing broken tool exchanges on the way (see the two repair sites below).
//
// opts carries the orphan-repair diagnostics (T4): the session/run ids and the
// per-message history origin, so a repair log line can localize the orphan by
// one entry. It is variadic and optional: callers that pass nothing (including
// the T1 trim integration path, which must NOT repair) get the zero options,
// meaning "no correlation, every message is persisted-origin".
func (a *sessionAgent) preparePrompt(msgs []message.Message, supportsImages bool, todos []session.Todo, attachments []message.Attachment, opts ...preparePromptOption) ([]fantasy.Message, []fantasy.FilePart) {
	var o preparePromptOptions
	for _, opt := range opts {
		opt(&o)
	}
	var history []fantasy.Message
	if !a.isSubAgent {
		history = append(history, fantasy.NewUserMessage(
			fmt.Sprintf(
				"<system_reminder>%s</system_reminder>",
				todoReminderText(todos),
			),
		))
	}
	// Collect all tool call IDs present in assistant messages and all tool
	// result IDs present in tool messages. This lets us detect both orphaned
	// tool results (result without a call) and orphaned tool calls (call
	// without a result).
	knownToolCallIDs := make(map[string]struct{})
	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			for _, tc := range m.ToolCalls() {
				knownToolCallIDs[tc.ID] = struct{}{}
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				knownToolResultIDs[tr.ToolCallID] = struct{}{}
			}
		}
	}

	for i, m := range msgs {
		// The history origin is carried positionally: origins[i] is this
		// message's origin, independent of its id (which may be empty or
		// duplicated and is not preserved one-to-one through the conversion).
		// It is resolved here, at the input index, and passed to the repair
		// sites so the line's origin is never looked up by id.
		origin := o.originAt(i)
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.ReasoningContent().String() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs, origin, o); ok {
				history = append(history, msg)
			}
			continue
		}
		aiMsgs := toAIMessage(&m)
		if !supportsImages {
			for i := range aiMsgs {
				if aiMsgs[i].Role == fantasy.MessageRoleUser {
					aiMsgs[i].Content = filterFileParts(aiMsgs[i].Content)
				}
			}
		}
		history = append(history, aiMsgs...)

		if m.Role == message.Assistant {
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs, origin, o); ok {
				history = append(history, msg)
			}
		}
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		if !supportsImages {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

// todoReminderText builds the system reminder describing the session's
// current todo list. An empty list gets the "currently empty" nudge toward
// the "todos" tool; a non-empty one is rendered back verbatim instead, so
// the model is never told a false thing about its own state (see
// preparePrompt).
func todoReminderText(todos []session.Todo) string {
	if len(todos) == 0 {
		return `This is a reminder that your todo list is currently empty. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`
	}

	var b strings.Builder
	b.WriteString("This is a reminder of your current todo list. DO NOT mention this to the user explicitly because they are already aware.\n")
	for _, todo := range todos {
		mark := " "
		if todo.Status == session.TodoStatusCompleted {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", mark, todo.Content)
	}
	b.WriteString("Continue working through it with the \"todos\" tool as items complete or change. Again do not mention this message to the user.")
	return b.String()
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// filterOrphanedToolResults converts a tool message to a fantasy.Message,
// dropping any tool result parts whose tool_call_id has no matching tool call
// in the known set. An orphaned result causes API validation to fail on every
// subsequent turn, permanently locking the session. Returns the filtered
// message and true if at least one valid part remains.
//
// Each dropped part is logged as an orphan repair (T4) carrying the session/run
// ids, the message id, the history origin (persisted/carried/summary) carried
// in positionally via origin, the repair action, the tool_call_id, and the
// running drop counter. The repair is a no-op on the returned history except
// for the dropped part.
func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}, origin historyOrigin, o preparePromptOptions) (fantasy.Message, bool) {
	aiMsgs := toAIMessage(&m)
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
	}
	// The fantasy part carries only the tool_call_id, not the tool name, so the
	// repair line resolves the name from the original message's results (the
	// name is authorship metadata, safe to log; the result content is not).
	toolNames := make(map[string]string, len(m.ToolResults()))
	for _, tr := range m.ToolResults() {
		toolNames[tr.ToolCallID] = tr.Name
	}
	var validParts []fantasy.MessagePart
	for _, part := range aiMsgs[0].Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			validParts = append(validParts, part)
			continue
		}
		if _, known := knownToolCallIDs[tr.ToolCallID]; known {
			validParts = append(validParts, part)
		} else {
			recordRepair("Dropping orphaned tool result with no matching tool call",
				o, m.ID, origin, repairDropResult, tr.ToolCallID, toolNames[tr.ToolCallID])
		}
	}
	if len(validParts) == 0 {
		return fantasy.Message{}, false
	}
	msg := aiMsgs[0]
	msg.Content = validParts
	return msg, true
}

// syntheticToolResultsForOrphanedCalls returns a tool message containing
// synthetic tool results for any tool calls in the assistant message that
// have no matching result in knownToolResultIDs. LLM APIs require every
// tool_use to be immediately followed by a tool_result; an interrupted
// session can leave orphaned tool_use blocks that permanently lock the
// conversation. Returns the message and true if any synthetic results were
// produced.
//
// Each injected result is logged as an orphan repair (T4). An orphaned *call*
// is the interrupted-stream signature (the call was emitted but its result
// never arrived), so the line is tagged interrupted=true, and its repair
// action is inject_result. origin is the positionally-carried history origin
// for the message index the orphan belongs to.
func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}, origin historyOrigin, o preparePromptOptions) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		recordRepair("Injecting synthetic tool result for orphaned tool call",
			o, m.ID, origin, repairInjectResult, tc.ID, tc.Name)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
}

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// We use the session hash is used instead of the raw UUID so the header
// value is deterministic and opaque.
func sessionHeaders(sessionID string) map[string]string {
	hash := session.HashID(sessionID)
	return map[string]string{
		"x-session-id":       hash,
		"x-session-affinity": hash,
	}
}

// convertToToolResult converts a fantasy tool result to a message tool result.
func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}

// sanitizeToolInput validates tool call JSON from the provider.
// Malformed input is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn(
			"Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	return input, false
}
