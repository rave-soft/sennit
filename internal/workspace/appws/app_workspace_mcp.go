package appws

import (
	"context"
	"errors"
	"fmt"

	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/workspace"
)

// -- MCP operations --

func (w *AppWorkspace) WaitForMCPInit(ctx context.Context) error {
	return w.app.MCP.WaitForInit(ctx)
}

func (w *AppWorkspace) MCPGetStates() map[string]workspace.MCPClientInfo {
	states := w.app.MCP.GetStates()
	result := make(map[string]workspace.MCPClientInfo, len(states))
	for name, state := range states {
		result[name] = workspace.MCPClientInfo{Name: state.Name, State: workspace.MCPState(state.State), Error: state.Error, Counts: workspace.MCPCounts{Tools: state.Counts.Tools, Prompts: state.Counts.Prompts, Resources: state.Counts.Resources}, ConnectedAt: state.ConnectedAt}
	}
	return result
}

func (w *AppWorkspace) MCPResources() []workspace.MCPResourceInfo {
	var result []workspace.MCPResourceInfo
	for mcpName, resources := range w.app.MCP.Resources() {
		for _, r := range resources {
			result = append(result, workspace.MCPResourceInfo{
				MCPName:  mcpName,
				URI:      r.URI,
				Title:    r.Name,
				MIMEType: r.MIMEType,
			})
		}
	}
	return result
}

func (w *AppWorkspace) MCPRefreshPrompts(ctx context.Context, name string) {
	w.app.MCP.RefreshPrompts(ctx, name)
}

func (w *AppWorkspace) MCPRefreshResources(ctx context.Context, name string) {
	w.app.MCP.RefreshResources(ctx, name)
}

func (w *AppWorkspace) RefreshMCPTools(ctx context.Context, name string) {
	w.app.MCP.RefreshTools(ctx, w.store, name)
}

func (w *AppWorkspace) ReadMCPResource(ctx context.Context, name, uri string) ([]workspace.MCPResourceContents, error) {
	contents, err := w.app.MCP.ReadResource(ctx, w.store, name, uri)
	if err != nil {
		return nil, err
	}
	result := make([]workspace.MCPResourceContents, len(contents))
	for i, c := range contents {
		result[i] = workspace.MCPResourceContents{
			URI:      c.URI,
			MIMEType: c.MIMEType,
			Text:     c.Text,
			Blob:     c.Blob,
		}
	}
	return result, nil
}

func (w *AppWorkspace) ListMCPPrompts(context.Context) ([]commands.MCPPrompt, error) {
	return commands.LoadMCPPrompts(w.app.MCP)
}

func (w *AppWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return commands.GetMCPPrompt(w.app.MCP, w.store, clientID, promptID, args)
}

func (w *AppWorkspace) EnableDockerMCP(ctx context.Context) error {
	mcpConfig, err := w.store.PrepareDockerMCPConfig()
	if err != nil {
		return err
	}

	if err := w.app.MCP.InitializeSingle(ctx, config.DockerMCPName, w.store); err != nil {
		disableErr := w.app.MCP.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("failed to start docker MCP: %w", errors.Join(err, disableErr))
	}

	if err := w.store.PersistDockerMCPConfig(mcpConfig); err != nil {
		disableErr := w.app.MCP.DisableSingle(w.store, config.DockerMCPName)
		w.store.RemoveDockerMCPInMemory()
		return fmt.Errorf("docker MCP started but failed to persist configuration: %w", errors.Join(err, disableErr))
	}

	return nil
}

func (w *AppWorkspace) DisableDockerMCP() error {
	if err := w.app.MCP.DisableSingle(w.store, config.DockerMCPName); err != nil {
		return fmt.Errorf("failed to disable docker MCP: %w", err)
	}
	return w.store.DisableDockerMCP()
}

func (w *AppWorkspace) MCPAuthenticate(ctx context.Context, name string) error {
	return w.app.MCP.AuthenticateMCP(ctx, w.store, name)
}

func (w *AppWorkspace) MCPPendingAuth() []workspace.MCPPendingAuthServer {
	pending := w.app.MCP.PendingAuthMCPs(w.store)
	result := make([]workspace.MCPPendingAuthServer, len(pending))
	for i, server := range pending {
		result[i] = workspace.MCPPendingAuthServer{Name: server.Name, URL: server.URL}
	}
	return result
}

func (w *AppWorkspace) MCPAuthURL(name string) string {
	return w.app.MCP.MCPAuthURL(name)
}
