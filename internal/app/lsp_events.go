package app

import (
	"context"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/pubsub"
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
// process (the top-level workspace and any spawned thread's workspace)
// shared one LSP status table: a second workspace's LSP clients silently
// overwrote the first's, and GetLSPStates() had no way to answer "which
// workspace's LSP clients?". Embedded directly in App so each workspace
// owns its own.
type lspEvents struct {
	states *csync.Map[string, LSPClientInfo]
	broker *pubsub.Broker[LSPEvent]
	// writeMu serializes the get-modify-set sequences in updateLSPState and
	// updateLSPDiagnostics. csync.Map's own Get/Set are each atomic, but
	// the pair is not: an LSP state change and a diagnostics callback can
	// land concurrently for the same client, and without this a
	// diagnostics update read stale before a state update's Set can be
	// clobbered by that state update's own Set — silently losing the
	// diagnostics count.
	writeMu sync.Mutex
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
// err is nil at every call site today, but the parameter stays: the field
// it fills is rendered by the LSP block in internal/ui/model/lsp.go, so a
// failing start ought to be reported through here rather than the plumbing
// being torn out.
//
//nolint:unparam // see above; the sink for this value already exists in the UI
func (l *lspEvents) updateLSPState(name string, state lsp.ServerState, err error, client *lsp.Client) {
	l.writeMu.Lock()
	info := LSPClientInfo{
		Name:   name,
		State:  state,
		Error:  err,
		Client: client,
	}
	existing, hadExisting := l.states.Get(name)
	if state == lsp.StateReady {
		info.ConnectedAt = time.Now()
	} else if hadExisting {
		info.ConnectedAt = existing.ConnectedAt
	}
	// Carry the diagnostic count across, the same as ConnectedAt: a state
	// update knows nothing about diagnostics and must not answer for them.
	// Every tool call reaches Manager.Start, and a reusable client makes
	// that a synchronous callback into here (manager.go's startServer), so
	// treating "no count supplied" as zero republished 0 on every read and
	// every edit - which is why the header's error tally kept emptying
	// while the LSP block below it, reading the client's live counters,
	// still said seven.
	//
	// A terminal state is the exception: a server that stopped, never
	// started or failed has no diagnostics to speak of, and holding the
	// last count there would leave a dead server reporting errors.
	switch state {
	case lsp.StateStopped, lsp.StateUnstarted, lsp.StateError:
		info.DiagnosticCount = 0
	default:
		if hadExisting {
			info.DiagnosticCount = existing.DiagnosticCount
		}
	}
	l.states.Set(name, info)
	l.writeMu.Unlock()

	// Publish state change event
	l.broker.Publish(pubsub.UpdatedEvent, LSPEvent{
		Type:            LSPEventStateChanged,
		Name:            name,
		State:           state,
		Error:           err,
		DiagnosticCount: info.DiagnosticCount,
	})
}

// updateLSPDiagnostics updates the diagnostic count for an LSP client and
// publishes an event.
func (l *lspEvents) updateLSPDiagnostics(name string, diagnosticCount int) {
	l.writeMu.Lock()
	info, exists := l.states.Get(name)
	if exists {
		info.DiagnosticCount = diagnosticCount
		l.states.Set(name, info)
	}
	l.writeMu.Unlock()

	if exists {
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
