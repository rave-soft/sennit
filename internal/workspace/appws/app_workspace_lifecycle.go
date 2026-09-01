package appws

import (
	"github.com/rave-soft/sennit/internal/agent/notify"
	mcptools "github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/app/threadspawn"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- BackgroundJobs --

func (w *AppWorkspace) BackgroundJobCounts() shell.BackgroundJobCounts {
	return w.app.BackgroundShells.Counts()
}

// -- Lifecycle --

func (w *AppWorkspace) Subscribe(send func(any)) {
	w.app.Subscribe(func(msg any) { send(w.translateEvent(msg)) }, w.app.Shutdown)
}

// translateEvent adapts a message from app's event fan-in into the shape
// the TUI's Update() expects. Every source app.setupEvents wires in at
// construction already arrives pre-shaped; the one exception is thread
// events, forwarded raw by app.ForwardEvents (see
// internal/app/threadspawn/attach.go) as the
// pubsub.Event[thread.Event] the Manager itself publishes, because
// ForwardEvents is generic over T and has no way to convert on the way
// in. Convert here, at the UI-facing boundary, into
// pubsub.Event[proto.Thread] so threads_dock.go, thread_indicator.go,
// thread_completion.go and threads.go (the dashboard) see live updates
// instead of relying solely on their TTL-poll fallback. Any other
// message passes through unchanged.
func (w *AppWorkspace) translateEvent(msg any) any {
	switch e := msg.(type) {
	case pubsub.Event[notify.Notification]:
		return pubsub.Event[workspace.AgentNotification]{Type: e.Type, Payload: workspace.AgentNotification{SessionID: e.Payload.SessionID, SessionTitle: e.Payload.SessionTitle, Type: workspace.AgentNotificationType(e.Payload.Type), ProviderID: e.Payload.ProviderID, RunID: e.Payload.RunID, Message: e.Payload.Message, AWSSOCommand: e.Payload.AWSSOCommand, AWSSOURL: e.Payload.AWSSOURL}}
	case pubsub.Event[mcptools.Event]:
		var eventType workspace.MCPEventType
		switch e.Payload.Type {
		case mcptools.EventStateChanged:
			eventType = workspace.MCPEventStateChanged
		case mcptools.EventToolsListChanged:
			eventType = workspace.MCPEventToolsListChanged
		case mcptools.EventPromptsListChanged:
			eventType = workspace.MCPEventPromptsListChanged
		case mcptools.EventResourcesListChanged:
			eventType = workspace.MCPEventResourcesListChanged
		default:
			return nil
		}
		return pubsub.Event[workspace.MCPEvent]{Type: e.Type, Payload: workspace.MCPEvent{Type: eventType, Name: e.Payload.Name}}
	case pubsub.Event[app.LSPEvent]:
		return pubsub.Event[workspace.LSPEvent]{Type: e.Type, Payload: workspace.LSPEvent{Type: workspace.LSPEventType(e.Payload.Type), Name: e.Payload.Name, State: lspState(e.Payload.State), Error: e.Payload.Error, DiagnosticCount: e.Payload.DiagnosticCount}}
	}
	e, ok := msg.(pubsub.Event[thread.Event])
	if !ok {
		return msg
	}
	// The manager (still attached — it's what published this event) can
	// resolve the thread's live WorkspaceID.
	workspaceID := ""
	if mgr, ok := w.threadManager(); ok {
		workspaceID = mgr.WorkspaceID(e.Payload.Thread.ID)
	}
	pe := threadspawn.EventToProto(e.Payload, workspaceID)
	return pubsub.Event[proto.Thread]{
		Type:    threadEventPubsubType(pe.Type),
		Payload: pe.Thread,
	}
}

func (w *AppWorkspace) Shutdown() {
	w.app.Shutdown()
}
