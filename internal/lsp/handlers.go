package lsp

import (
	"context"
	"encoding/json"
	"log/slog"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/lsp/util"
)

// HandleWorkspaceConfiguration handles workspace configuration requests.
// The response array must have exactly one entry per requested item, in
// the same order (LSP spec: "the order of the returned configuration
// settings correspond to the order of the passed ConfigurationItems").
// Sennit does not track per-scope settings, so every entry is an empty
// object, but a server that asks for 2+ sections (e.g. gopls asking for
// both "gopls" and "build") would otherwise get a misaligned response —
// its N-th setting read would land on the wrong item.
func HandleWorkspaceConfiguration(_ context.Context, _ string, params json.RawMessage) (any, error) {
	var configParams protocol.ConfigurationParams
	if err := json.Unmarshal(params, &configParams); err != nil {
		return []map[string]any{{}}, nil
	}
	result := make([]map[string]any, len(configParams.Items))
	for i := range result {
		result[i] = map[string]any{}
	}
	return result, nil
}

// HandleWorkDoneProgressCreate handles server-initiated window/workDoneProgress/create
// requests. The client advertises window.workDoneProgress: true in its capabilities
// (see makeClientCapabilities in powernap), which per the LSP spec grants servers
// permission to send this request — so it must be answered, even as a no-op, or the
// server (e.g. typescript-language-server) treats the unhandled response as fatal and
// crashes. See github.com/charmbracelet/x issue tracking powernap capability gaps.
func HandleWorkDoneProgressCreate(_ context.Context, _ string, _ json.RawMessage) (any, error) {
	return nil, nil
}

// HandleRegisterCapability handles capability registration requests
func HandleRegisterCapability(_ context.Context, _ string, params json.RawMessage) (any, error) {
	var registerParams protocol.RegistrationParams
	if err := json.Unmarshal(params, &registerParams); err != nil {
		slog.Error("Error unmarshaling registration params", "error", err)
		return nil, err
	}

	for _, reg := range registerParams.Registrations {
		switch reg.Method {
		case "workspace/didChangeWatchedFiles":
			// Parse the registration options
			optionsJSON, err := json.Marshal(reg.RegisterOptions)
			if err != nil {
				slog.Error("Error marshaling registration options", "error", err)
				continue
			}
			var options protocol.DidChangeWatchedFilesRegistrationOptions
			if err := json.Unmarshal(optionsJSON, &options); err != nil {
				slog.Error("Error unmarshaling registration options", "error", err)
				continue
			}
			// Store the file watchers registrations
			notifyFileWatchRegistration(reg.ID, options.Watchers)
		}
	}
	return nil, nil
}

// HandleApplyEdit handles workspace edit requests. encoding is called at
// request time, not registration time: registerHandlers wires this up
// before Initialize completes (servers like typescript-language-server can
// send requests during the handshake itself), so a value captured up front
// would always be the pre-negotiation default instead of what the server
// actually negotiated (e.g. clangd's utf-8).
func HandleApplyEdit(encoding func() powernap.OffsetEncoding) func(_ context.Context, _ string, params json.RawMessage) (any, error) {
	return func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		var edit protocol.ApplyWorkspaceEditParams
		if err := json.Unmarshal(params, &edit); err != nil {
			return nil, err
		}

		err := util.ApplyWorkspaceEdit(edit.Edit, encoding())
		if err != nil {
			slog.Error("Error applying workspace edit", "error", err)
			return protocol.ApplyWorkspaceEditResult{Applied: false, FailureReason: err.Error()}, nil
		}

		return protocol.ApplyWorkspaceEditResult{Applied: true}, nil
	}
}

// FileWatchRegistrationHandler is a function that will be called when file watch registrations are received
type FileWatchRegistrationHandler func(id string, watchers []protocol.FileSystemWatcher)

// fileWatchHandler holds the current handler for file watch registrations
var fileWatchHandler FileWatchRegistrationHandler

// notifyFileWatchRegistration notifies the handler about new file watch registrations
func notifyFileWatchRegistration(id string, watchers []protocol.FileSystemWatcher) {
	if fileWatchHandler != nil {
		fileWatchHandler(id, watchers)
	}
}

// HandleServerMessage handles server messages
func HandleServerMessage(_ context.Context, method string, params json.RawMessage) {
	var msg protocol.ShowMessageParams
	if err := json.Unmarshal(params, &msg); err != nil {
		slog.Debug("Error unmarshal server message", "error", err)
		return
	}

	switch msg.Type {
	case protocol.Error:
		slog.Error("LSP Server", "message", msg.Message)
	case protocol.Warning:
		slog.Warn("LSP Server", "message", msg.Message)
	case protocol.Info:
		slog.Info("LSP Server", "message", msg.Message)
	case protocol.Log:
		slog.Debug("LSP Server", "message", msg.Message)
	}
}

// HandleDiagnostics handles diagnostic notifications from the LSP server
func HandleDiagnostics(client *Client, params json.RawMessage) {
	var diagParams protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &diagParams); err != nil {
		slog.Error("Error unmarshaling diagnostics params", "error", err)
		return
	}

	client.diagnostics.Set(diagParams.URI, diagParams.Diagnostics)

	// Calculate total diagnostic count
	totalCount := 0
	for _, diagnostics := range client.diagnostics.Seq2() {
		totalCount += len(diagnostics)
	}

	// Trigger callback if set
	if client.onDiagnosticsChanged != nil {
		client.onDiagnosticsChanged(client.name, totalCount)
	}
}
