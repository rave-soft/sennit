package appws

import (
	"context"

	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- LSP --

func (w *AppWorkspace) LSPStart(ctx context.Context, path string) {
	w.app.LSPManager.Start(ctx, path)
}

func (w *AppWorkspace) LSPStopAll(ctx context.Context) {
	w.app.LSPManager.StopAll(ctx)
}

func (w *AppWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	states := w.app.GetLSPStates()
	out := make(map[string]workspace.LSPClientInfo, len(states))
	for name, info := range states {
		out[name] = lspClientInfo(info)
	}
	return out
}

func (w *AppWorkspace) LSPGetDiagnosticCounts(name string) proto.LSPDiagnosticCounts {
	state, ok := w.app.GetLSPState(name)
	if !ok || state.Client == nil {
		return proto.LSPDiagnosticCounts{}
	}
	counts := state.Client.GetDiagnosticCounts()
	return proto.LSPDiagnosticCounts{
		Error:       counts.Error,
		Warning:     counts.Warning,
		Information: counts.Information,
		Hint:        counts.Hint,
	}
}

// lspClientInfo converts the runtime's view of a server into the one the
// frontend gets. The conversion is the point: ClientInfo carries a live
// *lsp.Client, and the contract used to alias the type straight through.
func lspClientInfo(info lsp.ClientInfo) proto.LSPClientInfo {
	out := proto.LSPClientInfo{
		Name:            info.Name,
		State:           lspState(info.State),
		DiagnosticCount: info.DiagnosticCount,
		ConnectedAt:     info.ConnectedAt,
	}
	if info.Error != nil {
		out.Error = info.Error.Error()
	}
	return out
}

// lspState maps the runtime's iota onto the transport's names. An unknown
// value reports "unstarted" rather than an empty string, so a frontend
// switch always has a case to land on.
func lspState(s lsp.ServerState) proto.LSPState {
	switch s {
	case lsp.StateStarting:
		return proto.LSPStateStarting
	case lsp.StateReady:
		return proto.LSPStateReady
	case lsp.StateError:
		return proto.LSPStateError
	case lsp.StateStopped:
		return proto.LSPStateStopped
	case lsp.StateDisabled:
		return proto.LSPStateDisabled
	default:
		return proto.LSPStateUnstarted
	}
}
