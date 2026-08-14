package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/thread"
)

// wrapEvent converts a raw tea.Msg (a pubsub.Event[T] from the app
// event fan-in) into a pubsub.Payload envelope with the correct
// PayloadType discriminator and a proto-typed inner payload that has
// proper JSON tags. Returns nil if the event type is unrecognized.
func wrapEvent(ev any) *pubsub.Payload {
	switch e := ev.(type) {
	case pubsub.Event[app.LSPEvent]:
		return envelope(pubsub.PayloadTypeLSPEvent, pubsub.Event[proto.LSPEvent]{
			Type: e.Type,
			Payload: proto.LSPEvent{
				Type:            proto.LSPEventType(e.Payload.Type),
				Name:            e.Payload.Name,
				State:           e.Payload.State,
				Error:           e.Payload.Error,
				DiagnosticCount: e.Payload.DiagnosticCount,
			},
		})
	case pubsub.Event[mcp.Event]:
		pt := mcpEventTypeToProto(e.Payload.Type)
		if pt == "" {
			// Unsupported MCP event type (e.g. EventChannelMessage, which
			// has no proto representation until session delivery is wired
			// up). Drop it instead of fabricating a state_changed event.
			slog.Debug("Dropping unsupported MCP event type for SSE", "type", e.Payload.Type)
			return nil
		}
		return envelope(pubsub.PayloadTypeMCPEvent, pubsub.Event[proto.MCPEvent]{
			Type: e.Type,
			Payload: proto.MCPEvent{
				Type:      pt,
				Name:      e.Payload.Name,
				State:     proto.MCPState(e.Payload.State),
				Error:     e.Payload.Error,
				ToolCount: e.Payload.Counts.Tools,
			},
		})
	case pubsub.Event[permission.PermissionRequest]:
		return envelope(pubsub.PayloadTypePermissionRequest, pubsub.Event[proto.PermissionRequest]{
			Type: e.Type,
			Payload: proto.PermissionRequest{
				ID:          e.Payload.ID,
				SessionID:   e.Payload.SessionID,
				ToolCallID:  e.Payload.ToolCallID,
				ToolName:    e.Payload.ToolName,
				Description: e.Payload.Description,
				Action:      e.Payload.Action,
				Path:        e.Payload.Path,
				Params:      e.Payload.Params,
				Delegation: proto.DelegationRef{
					ID:   e.Payload.Delegation.ID,
					Name: e.Payload.Delegation.Name,
					Kind: e.Payload.Delegation.Kind,
				},
			},
		})
	case pubsub.Event[permission.PermissionNotification]:
		return envelope(pubsub.PayloadTypePermissionNotification, pubsub.Event[proto.PermissionNotification]{
			Type: e.Type,
			Payload: proto.PermissionNotification{
				ToolCallID: e.Payload.ToolCallID,
				Granted:    e.Payload.Granted,
				Denied:     e.Payload.Denied,
			},
		})
	case pubsub.Event[question.Request]:
		slog.Info("Wrapping question batch event for SSE", "id", e.Payload.ID, "questions", len(e.Payload.Questions))
		return envelope(pubsub.PayloadTypeQuestionRequest, pubsub.Event[proto.QuestionRequest]{
			Type: e.Type,
			Payload: proto.QuestionRequest{
				ID:                 e.Payload.ID,
				SessionID:          e.Payload.SessionID,
				ToolCallID:         e.Payload.ToolCallID,
				Questions:          questionsToProto(e.Payload.Questions),
				ConfirmTitle:       e.Payload.ConfirmTitle,
				ConfirmDescription: e.Payload.ConfirmDescription,
			},
		})
	case pubsub.Event[question.Notification]:
		return envelope(pubsub.PayloadTypeQuestionNotification, pubsub.Event[proto.QuestionNotification]{
			Type: e.Type,
			Payload: proto.QuestionNotification{
				BatchID: e.Payload.BatchID,
			},
		})
	case pubsub.Event[message.Message]:
		return envelope(pubsub.PayloadTypeMessage, pubsub.Event[proto.Message]{
			Type:    e.Type,
			Payload: messageToProto(e.Payload),
		})
	case pubsub.Event[session.Session]:
		return envelope(pubsub.PayloadTypeSession, pubsub.Event[proto.Session]{
			Type:    e.Type,
			Payload: sessionToProto(e.Payload),
		})
	case pubsub.Event[history.File]:
		return envelope(pubsub.PayloadTypeFile, pubsub.Event[proto.File]{
			Type:    e.Type,
			Payload: fileToProto(e.Payload),
		})
	case pubsub.Event[notify.Notification]:
		payload := proto.AgentEvent{
			SessionID:    e.Payload.SessionID,
			SessionTitle: e.Payload.SessionTitle,
			RunID:        e.Payload.RunID,
			Type:         proto.AgentEventType(e.Payload.Type),
			AWSSOCommand: e.Payload.AWSSOCommand,
			AWSSOURL:     e.Payload.AWSSOURL,
		}
		// Carry any human-readable message across the wire; the client
		// maps Error back into Notification.Message.
		if e.Payload.Message != "" {
			payload.Error = errors.New(e.Payload.Message)
		}
		if e.Payload.Type == notify.TypeAgentError {
			payload.Type = proto.AgentEventTypeError
		}
		return envelope(pubsub.PayloadTypeAgentEvent, pubsub.Event[proto.AgentEvent]{
			Type:    e.Type,
			Payload: payload,
		})
	case pubsub.Event[notify.RunComplete]:
		return envelope(pubsub.PayloadTypeRunComplete, pubsub.Event[proto.RunComplete]{
			Type: e.Type,
			Payload: proto.RunComplete{
				SessionID: e.Payload.SessionID,
				RunID:     e.Payload.RunID,
				MessageID: e.Payload.MessageID,
				Text:      e.Payload.Text,
				Error:     e.Payload.Error,
				Cancelled: e.Payload.Cancelled,
			},
		})
	case pubsub.Event[proto.ConfigChanged]:
		return envelope(pubsub.PayloadTypeConfigChanged, e)
	case pubsub.Event[proto.ServerNotice]:
		return envelope(pubsub.PayloadTypeServerNotice, e)
	case app.UpdateAvailableMsg:
		return envelope(pubsub.PayloadTypeUpdateAvailable, pubsub.Event[proto.UpdateAvailable]{
			Type: pubsub.UpdatedEvent,
			Payload: proto.UpdateAvailable{
				CurrentVersion: e.CurrentVersion,
				LatestVersion:  e.LatestVersion,
				IsDevelopment:  e.IsDevelopment,
			},
		})
	case pubsub.Event[skills.Event]:
		return envelope(pubsub.PayloadTypeSkillsEvent, pubsub.Event[proto.SkillsEvent]{
			Type:    e.Type,
			Payload: skillsEventToProto(e.Payload),
		})
	case pubsub.Event[thread.Event]:
		// This event was forwarded raw from Manager.Subscribe (see
		// app.ForwardEvents in thread.Attach), so wrapEvent has no *thread.Manager
		// reference here to resolve a live WorkspaceID from — pass "" and
		// let clients that need it re-GET the thread.
		return envelope(pubsub.PayloadTypeThreadEvent, pubsub.Event[proto.ThreadEvent]{
			Type:    e.Type,
			Payload: thread.EventToProto(e.Payload, ""),
		})
	default:
		slog.Warn("Unrecognized event type for SSE wrapping", "type", fmt.Sprintf("%T", ev))
		return nil
	}
}

// envelope marshals the inner event and wraps it in a pubsub.Payload.
func envelope(payloadType pubsub.PayloadType, inner any) *pubsub.Payload {
	raw, err := json.Marshal(inner)
	if err != nil {
		slog.Error("Failed to marshal event payload", "error", err)
		return nil
	}
	return &pubsub.Payload{
		Type:    payloadType,
		Payload: raw,
	}
}

func mcpEventTypeToProto(t mcp.EventType) proto.MCPEventType {
	switch t {
	case mcp.EventStateChanged:
		return proto.MCPEventStateChanged
	case mcp.EventToolsListChanged:
		return proto.MCPEventToolsListChanged
	case mcp.EventPromptsListChanged:
		return proto.MCPEventPromptsListChanged
	case mcp.EventResourcesListChanged:
		return proto.MCPEventResourcesListChanged
	default:
		// Unsupported type (e.g. EventChannelMessage). Return empty so
		// callers can drop it rather than coercing to state_changed.
		return ""
	}
}

func sessionToProto(s session.Session) proto.Session {
	return proto.Session{
		ID:               s.ID,
		ParentSessionID:  s.ParentSessionID,
		Title:            s.Title,
		SummaryMessageID: s.SummaryMessageID,
		MessageCount:     s.MessageCount,
		PromptTokens:     s.PromptTokens,
		CompletionTokens: s.CompletionTokens,
		Cost:             s.Cost,
		Todos:            s.Todos,
		CreatedAt:        s.CreatedAt,
		UpdatedAt:        s.UpdatedAt,
	}
}

// isSessionBusy reports whether the given workspace has an in-flight
// agent run for sessionID. It tolerates a nil workspace (treating it as
// "not busy") so REST handlers can pass GetWorkspace's result through
// unconditionally — the workspace lookup error is already surfaced by
// the prior ListSessions/GetSession call when relevant.
func isSessionBusy(ws *backend.Workspace, sessionID string) bool {
	if ws == nil || ws.App == nil || ws.AgentCoordinator == nil {
		return false
	}
	return ws.AgentCoordinator.IsSessionBusy(sessionID)
}

// attachedClients returns the number of clients currently viewing
// sessionID in ws. Hold-only clients (streams == 0) do not contribute.
// A nil workspace is treated as zero so handlers can pass GetWorkspace's
// result through without an extra guard.
func attachedClients(ws *backend.Workspace, sessionID string) int {
	if ws == nil {
		return 0
	}
	return ws.AttachedClientsForSession(sessionID)
}

func fileToProto(f history.File) proto.File {
	return proto.File{
		ID:        f.ID,
		SessionID: f.SessionID,
		Path:      f.Path,
		Content:   f.Content,
		Version:   f.Version,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}

// messageToProto converts a [message.Message] to its wire form. This
// used to be a 9-way type switch rebuilding every content part by
// hand; now that proto's content part types are aliases of message's
// (see proto/message.go), a [message.Message]'s Parts slice already
// *is* a []proto.ContentPart, so there's nothing left to convert.
func messageToProto(m message.Message) proto.Message {
	return proto.Message{
		ID:        m.ID,
		SessionID: m.SessionID,
		Role:      m.Role,
		Parts:     m.Parts,
		Model:     m.Model,
		Provider:  m.Provider,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

// skillsEventToProto converts a skills.Event into its wire form. Errors
// are flattened to strings because error does not round-trip over JSON.
func skillsEventToProto(e skills.Event) proto.SkillsEvent {
	if len(e.States) == 0 {
		return proto.SkillsEvent{}
	}
	out := proto.SkillsEvent{States: make([]proto.SkillState, len(e.States))}
	for i, s := range e.States {
		entry := proto.SkillState{
			Name:  s.Name,
			Path:  s.Path,
			State: proto.SkillDiscoveryState(s.State),
		}
		if s.Err != nil {
			entry.Error = s.Err.Error()
		}
		out.States[i] = entry
	}
	return out
}

func messagesToProto(msgs []message.Message) []proto.Message {
	out := make([]proto.Message, len(msgs))
	for i, m := range msgs {
		out[i] = messageToProto(m)
	}
	return out
}

func questionsToProto(qs []question.Question) []proto.QuestionItem {
	if len(qs) == 0 {
		return nil
	}
	out := make([]proto.QuestionItem, len(qs))
	for i, q := range qs {
		choices := make([]proto.QuestionChoice, len(q.Choices))
		for j, c := range q.Choices {
			choices[j] = proto.QuestionChoice{
				ID:          c.ID,
				Label:       c.Label,
				Description: c.Description,
			}
		}
		out[i] = proto.QuestionItem{
			ID:          q.ID,
			Type:        string(q.Type),
			Label:       q.Label,
			Question:    q.Text,
			Description: q.Description,
			Choices:     choices,
		}
	}
	return out
}
