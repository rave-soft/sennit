package backend

import (
	"context"

	mcptools "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/pubsub"
)

// SubscribeEvents returns a per-caller event channel for a workspace.
// Each caller receives all events; multiple callers do not compete.
func (b *Backend) SubscribeEvents(ctx context.Context, workspaceID string) (<-chan pubsub.Event[any], error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Events(ctx), nil
}

// GetLSPStates returns the state of workspaceID's own LSP clients.
func (b *Backend) GetLSPStates(workspaceID string) (map[string]app.LSPClientInfo, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.GetLSPStates(), nil
}

// GetLSPDiagnostics returns diagnostics for a specific LSP client in
// the workspace.
func (b *Backend) GetLSPDiagnostics(workspaceID, lspName string) (any, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	for name, client := range ws.LSPManager.Clients().Seq2() {
		if name == lspName {
			return client.GetDiagnostics(), nil
		}
	}

	return nil, ErrLSPClientNotFound
}

// GetWorkspaceConfig returns the workspace-level configuration.
func (b *Backend) GetWorkspaceConfig(workspaceID string) (*config.Config, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Cfg.Config(), nil
}

// GetWorkspaceProviders returns the configured providers for a
// workspace.
func (b *Backend) GetWorkspaceProviders(workspaceID string) (any, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}

	return ws.Cfg.KnownProviders(), nil
}

// LSPStart starts an LSP server for the given path.
func (b *Backend) LSPStart(ctx context.Context, workspaceID, path string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	ws.LSPManager.Start(ctx, path)
	return nil
}

// LSPStopAll stops all LSP servers for a workspace.
func (b *Backend) LSPStopAll(ctx context.Context, workspaceID string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}

	ws.LSPManager.StopAll(ctx)
	return nil
}

// MCPGetStates returns the current state of all MCP clients belonging to
// workspaceID.
func (b *Backend) MCPGetStates(workspaceID string) map[string]mcptools.ClientInfo {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil
	}
	return ws.MCP.GetStates()
}

// MCPRefreshPrompts refreshes prompts for a named MCP client.
func (b *Backend) MCPRefreshPrompts(ctx context.Context, workspaceID string, name string) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return
	}
	ws.MCP.RefreshPrompts(ctx, name)
}

// MCPRefreshResources refreshes resources for a named MCP client.
func (b *Backend) MCPRefreshResources(ctx context.Context, workspaceID string, name string) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return
	}
	ws.MCP.RefreshResources(ctx, name)
}

// MCPPendingAuth returns the MCP servers awaiting OAuth authentication,
// for clients that need to prompt the user. workspaceID selects the
// workspace whose config provides the server URLs.
func (b *Backend) MCPPendingAuth(workspaceID string) ([]mcptools.PendingAuthServer, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	return ws.MCP.PendingAuthMCPs(ws.Cfg), nil
}

// MCPAuthURL returns the current OAuth authorization URL for a named
// server on workspaceID, if a flow is in progress.
func (b *Backend) MCPAuthURL(workspaceID, name string) string {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return ""
	}
	return ws.MCP.MCPAuthURL(name)
}

// MCPAuthenticate runs the OAuth flow for a named MCP server with the
// local browser suppressed: the authorization URL is exposed via
// MCPAuthURL/MCPPendingAuth for the calling client to open on the user's
// machine. The call blocks until the flow completes, fails, or ctx is
// cancelled. workspaceID selects the workspace whose config drives the
// flow.
func (b *Backend) MCPAuthenticate(ctx context.Context, workspaceID, name string) error {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return err
	}
	finish, cancel, err := ws.MCP.BeginAuth(ws.Cfg, name)
	if err != nil {
		return err
	}
	defer cancel()
	return finish(ctx)
}
