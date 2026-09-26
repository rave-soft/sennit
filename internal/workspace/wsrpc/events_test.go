package wsrpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/wireerr"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// This file's samples are wsrpc's own, not a reuse of
// internal/workspace's (that file is package workspace's own _test.go
// and unexported, so it cannot be imported from here) -- see PR 0.7's
// build step 1. They follow the same recipe: a fixed non-monotonic UTC
// time and every field populated to a non-zero value, so a round trip
// that silently drops a field shows up as a require.Equal failure
// instead of two zero values comparing equal by accident.
var eventSampleTime = time.Date(2026, 3, 4, 15, 6, 7, 0, time.UTC)

var eventSampleWireErr = wireerr.Error{
	Code:    "internal",
	Message: "boom",
}

var eventSampleMessage = pubsub.Event[message.Message]{
	Type: pubsub.UpdatedEvent,
	Payload: message.Message{
		ID:        "msg-1",
		Role:      message.Assistant,
		SessionID: "sess-1",
		Parts:     []message.ContentPart{message.TextContent{Text: "hi"}},
		Model:     "gpt-5",
		Provider:  "openai",
		CreatedAt: eventSampleTime.Unix(),
		UpdatedAt: eventSampleTime.Unix(),
	},
}

var eventSampleSession = pubsub.Event[session.Session]{
	Type: pubsub.CreatedEvent,
	Payload: session.Session{
		ID:              "sess-1",
		ParentSessionID: "parent-sess-1",
		Model:           session.ModelRef{Provider: "openai", Model: "gpt-5"},
		AgentID:         "reviewer",
		Title:           "Fix the bug",
		MessageCount:    5,
		Cost:            0.42,
		CreatedAt:       eventSampleTime.Unix(),
		UpdatedAt:       eventSampleTime.Unix(),
	},
}

var eventSampleThread = pubsub.Event[proto.Thread]{
	Type: pubsub.UpdatedEvent,
	Payload: proto.Thread{
		ID:           "thread-1",
		Name:         "fix-bug",
		Goal:         "fix the bug",
		BaseBranch:   "main",
		Branch:       "sennit/fix-bug",
		WorktreePath: "/repo/.sennit/threads/fix-bug",
		WorkspaceID:  "ws-1",
		SessionID:    "sess-1",
		Status:       "failed",
		Kind:         "thread",
		Error:        "exit status 1",
		CreatedAt:    eventSampleTime.Unix(),
		UpdatedAt:    eventSampleTime.Unix(),
	},
}

var eventSamplePermissionRequest = pubsub.Event[permission.PermissionRequest]{
	Type: pubsub.CreatedEvent,
	Payload: permission.PermissionRequest{
		ID:          "perm-1",
		SessionID:   "sess-1",
		ToolCallID:  "call-1",
		ToolName:    proto.BashToolName,
		Description: "run a command",
		Action:      "execute",
		Params: proto.BashPermissionsParams{
			Description: "run a command",
			Command:     "echo hi",
			WorkingDir:  "/repo",
		},
		Path: "/repo",
	},
}

var eventSamplePermissionNotification = pubsub.Event[permission.PermissionNotification]{
	Type: pubsub.UpdatedEvent,
	Payload: permission.PermissionNotification{
		ToolCallID: "call-1",
		Granted:    true,
	},
}

var eventSampleQuestionRequest = pubsub.Event[question.Request]{
	Type: pubsub.CreatedEvent,
	Payload: question.Request{
		ID:         "batch-1",
		SessionID:  "sess-1",
		ToolCallID: "call-1",
		Questions: []question.Question{{
			ID:   "q-1",
			Type: question.TypeSingleChoice,
			Text: "Should I proceed?",
			Choices: []question.Choice{
				{ID: "choice-1", Label: "Yes"},
			},
		}},
	},
}

var eventSampleQuestionNotification = pubsub.Event[question.Notification]{
	Type:    pubsub.DeletedEvent,
	Payload: question.Notification{BatchID: "batch-1"},
}

var eventSampleHistoryFile = pubsub.Event[history.File]{
	Type: pubsub.CreatedEvent,
	Payload: history.File{
		ID:        "hist-1",
		SessionID: "sess-1",
		Path:      "internal/foo.go",
		Content:   "package foo",
		Version:   2,
		CreatedAt: eventSampleTime.Unix(),
		UpdatedAt: eventSampleTime.Unix(),
	},
}

var eventSampleSkillsEvent = pubsub.Event[skills.Event]{
	Type: pubsub.UpdatedEvent,
	Payload: skills.Event{
		States: []*skills.SkillState{{
			Name:  "reviewer",
			Path:  "/repo/.sennit/skills/reviewer",
			State: skills.StateError,
			Err:   &eventSampleWireErr,
		}},
	},
}

var eventSampleAgentNotification = pubsub.Event[workspace.AgentNotification]{
	Type: pubsub.CreatedEvent,
	Payload: workspace.AgentNotification{
		SessionID:    "sess-1",
		SessionTitle: "Fix the bug",
		ChildSession: true,
		Type:         workspace.AgentNotificationFinished,
		ProviderID:   "openai",
		RunID:        "run-1",
		Message:      "done",
	},
}

var eventSampleLSPEvent = pubsub.Event[workspace.LSPEvent]{
	Type: pubsub.UpdatedEvent,
	Payload: workspace.LSPEvent{
		Type:            workspace.LSPEventStateChanged,
		Name:            "gopls",
		State:           proto.LSPStateReady,
		Error:           &eventSampleWireErr,
		DiagnosticCount: 3,
	},
}

var eventSampleMCPEvent = pubsub.Event[workspace.MCPEvent]{
	Type: pubsub.UpdatedEvent,
	Payload: workspace.MCPEvent{
		Type: workspace.MCPEventStateChanged,
		Name: "myserver",
	},
}

// eventSamples pairs every registered event with its populated sample, so
// TestEventRegistry_RoundTrips can enumerate the registry (not this list)
// and still catch a sample nobody bothered to add: it fails loudly if a
// registered name has no entry here, rather than silently skipping it.
var eventSamples = map[string]any{
	"message":                 eventSampleMessage,
	"session":                 eventSampleSession,
	"thread":                  eventSampleThread,
	"permission_request":      eventSamplePermissionRequest,
	"permission_notification": eventSamplePermissionNotification,
	"question_request":        eventSampleQuestionRequest,
	"question_notification":   eventSampleQuestionNotification,
	"history_file":            eventSampleHistoryFile,
	"skills":                  eventSampleSkillsEvent,
	"agent_notification":      eventSampleAgentNotification,
	"lsp":                     eventSampleLSPEvent,
	"mcp":                     eventSampleMCPEvent,
}

// TestEventRegistry_RoundTrips enumerates the registry built in events.go
// (not eventSamples) and requires a populated sample for each -- a
// registered type with nothing here fails immediately, naming it, rather
// than silently passing zero tests for it. Each sample is EncodeEvent'd,
// re-parsed as JSON (proving the wire shape is snake_case, not just
// Go-JSON-roundtrippable), then DecodeEvent'd, and must come back equal
// to the original.
func TestEventRegistry_RoundTrips(t *testing.T) {
	t.Parallel()

	require.Len(t, eventSamples, len(eventsByName), "eventSamples must cover exactly the registered event names")

	for name, entry := range eventsByName {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sample, ok := eventSamples[name]
			require.True(t, ok, "no sample registered for event %q; add one to eventSamples", name)
			require.Equal(t, entry.name, name)

			env, err := EncodeEvent(sample)
			require.NoError(t, err)
			require.Equal(t, name, env.Type)

			// Prove the payload is genuine JSON with the field's own tags
			// (snake_case), not just something Go's own codec can read
			// back -- unmarshal into a bare map and check a couple of
			// known keys exist, the same way wire_dto_test.go's secret
			// scan proves shape rather than trusting the Go-side round
			// trip alone.
			var asMap map[string]any
			require.NoError(t, json.Unmarshal(env.Payload, &asMap))
			require.Contains(t, asMap, "type")
			require.Contains(t, asMap, "payload")

			decoded, err := DecodeEvent(env)
			require.NoError(t, err)
			require.Equal(t, sample, decoded)
		})
	}
}

// TestEventRegistry_EncodeUnknownType requires EncodeEvent to name the
// unregistered Go type in its error rather than failing silently.
func TestEventRegistry_EncodeUnknownType(t *testing.T) {
	t.Parallel()

	_, err := EncodeEvent(pubsub.Event[int]{Payload: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pubsub.Event[int]")
}

// TestEventRegistry_DecodeUnknownName requires DecodeEvent to name the
// unregistered wire name in its error rather than failing silently.
func TestEventRegistry_DecodeUnknownName(t *testing.T) {
	t.Parallel()

	_, err := DecodeEvent(Envelope{Type: "no-such-event", Payload: []byte("{}")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no-such-event")
}
