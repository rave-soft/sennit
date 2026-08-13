package app

import (
	"context"
	"time"

	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/pubsub"
)

// LSPEventType represents the type of LSP event
type LSPEventType string

const (
	LSPEventStateChanged       LSPEventType = "state_changed"
	LSPEventDiagnosticsChanged LSPEventType = "diagnostics_changed"
)

// LSPEvent represents an event in the LSP system
type LSPEvent struct {
	Type            LSPEventType
	Name            string
	State           lsp.ServerState
	Error           error
	DiagnosticCount int
}

// LSPClientInfo aliases the canonical LSP client state.
type LSPClientInfo = lsp.ClientInfo

// lspEvents holds one workspace's LSP client state and event broker. It
// used to be a pair of package-level vars, which meant every App in a
// process (multi-client backend mode) shared one LSP status table: a
// second workspace's LSP clients silently overwrote the first's, and
// GetLSPStates() had no way to answer "which workspace's LSP clients?".
// Embedded directly in App so each workspace owns its own (see
// ARCHITECTURE_REVIEW.md section 3.1).
type lspEvents struct {
	states *csync.Map[string, LSPClientInfo]
	broker *pubsub.Broker[LSPEvent]
}

func newLSPEvents() *lspEvents {
	return &lspEvents{
		states: csync.NewMap[string, LSPClientInfo](),
		broker: pubsub.NewBroker[LSPEvent](),
	}
}

// SubscribeLSPEvents returns a channel for this workspace's LSP events.
func (l *lspEvents) SubscribeLSPEvents(ctx context.Context) <-chan pubsub.Event[LSPEvent] {
	return l.broker.Subscribe(ctx)
}

// GetLSPStates returns the current state of all of this workspace's LSP
// clients.
func (l *lspEvents) GetLSPStates() map[string]LSPClientInfo {
	return l.states.Copy()
}

// GetLSPState returns the state of a specific LSP client.
func (l *lspEvents) GetLSPState(name string) (LSPClientInfo, bool) {
	return l.states.Get(name)
}

// updateLSPState updates the state of an LSP client and publishes an event.
func (l *lspEvents) updateLSPState(name string, state lsp.ServerState, err error, client *lsp.Client, diagnosticCount int) {
	info := LSPClientInfo{
		Name:            name,
		State:           state,
		Error:           err,
		Client:          client,
		DiagnosticCount: diagnosticCount,
	}
	if state == lsp.StateReady {
		info.ConnectedAt = time.Now()
	} else if existing, ok := l.states.Get(name); ok {
		info.ConnectedAt = existing.ConnectedAt
	}
	l.states.Set(name, info)

	// Publish state change event
	l.broker.Publish(pubsub.UpdatedEvent, LSPEvent{
		Type:            LSPEventStateChanged,
		Name:            name,
		State:           state,
		Error:           err,
		DiagnosticCount: diagnosticCount,
	})
}

// updateLSPDiagnostics updates the diagnostic count for an LSP client and
// publishes an event.
func (l *lspEvents) updateLSPDiagnostics(name string, diagnosticCount int) {
	if info, exists := l.states.Get(name); exists {
		info.DiagnosticCount = diagnosticCount
		l.states.Set(name, info)

		// Publish diagnostics change event
		l.broker.Publish(pubsub.UpdatedEvent, LSPEvent{
			Type:            LSPEventDiagnosticsChanged,
			Name:            name,
			State:           info.State,
			Error:           info.Error,
			DiagnosticCount: diagnosticCount,
		})
	}
}
