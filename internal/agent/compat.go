package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/vercel"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/stringext"
)

func (a *sessionAgent) preparePrompt(msgs []message.Message, supportsImages bool, todos []session.Todo, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
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

	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.ReasoningContent().String() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs); ok {
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
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs); ok {
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
func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	aiMsgs := toAIMessage(&m)
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
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
			slog.Warn(
				"Dropping orphaned tool result with no matching tool call",
				"tool_call_id", tr.ToolCallID,
			)
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
func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", tc.ID,
			"tool_name", tc.Name,
		)
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

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, model Model) []fantasy.Message {
	providerSupportsMedia := model.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock) ||
		model.ModelCfg.Provider == string(catwalk.InferenceProviderBedrockEurope)

	if providerSupportsMedia {
		return messages
	}

	supportsImages := model.CatalogCfg.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	return cacheControlOptions()
}

func cacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv(brand.EnvPrefix + "DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	return fantasy.ProviderOptions{
		anthropic.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		bedrock.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
		vercel.Name: &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		},
	}
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
