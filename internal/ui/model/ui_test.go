package model

import (
	"context"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestCurrentModelSupportsImages(t *testing.T) {
	t.Parallel()

	t.Run("returns false when config is nil", func(t *testing.T) {
		t.Parallel()

		ui := newTestUIWithConfig(t, nil)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when coder agent is missing", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents:    map[string]config.Agent{},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns false when model is not found", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{
			Providers: csync.NewMap[string, config.ProviderConfig](),
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: string(config.SelectedModelTypeLarge)},
			},
		}
		ui := newTestUIWithConfig(t, cfg)
		require.False(t, ui.currentModelSupportsImages())
	})

	t.Run("returns true when current model supports images", func(t *testing.T) {
		t.Parallel()

		providers := csync.NewMap[string, config.ProviderConfig]()
		providers.Set("test-provider", config.ProviderConfig{
			ID: "test-provider",
			Models: []catwalk.Model{
				{ID: "test-model", SupportsImages: true},
			},
		})

		cfg := &config.Config{
			Model: config.SelectedModel{
				Provider: "test-provider",
				Model:    "test-model",
			},
			Providers: providers,
			Agents: map[string]config.Agent{
				config.AgentCoder: {Model: string(config.SelectedModelTypeLarge)},
			},
		}

		ui := newTestUIWithConfig(t, cfg)
		require.True(t, ui.currentModelSupportsImages())
	})
}

func newTestUIWithConfig(t *testing.T, cfg *config.Config) *UI {
	t.Helper()

	return &UI{
		com: &common.Common{
			Workspace: &testWorkspace{cfg: cfg},
		},
	}
}

// testWorkspace is a minimal [workspace.Workspace] stub for unit tests. It
// must embed the full Workspace interface (rather than one of the role
// interfaces in package workspace) because it is stored in
// common.Common.Workspace, the single DI field shared by the whole UI
// (dialogs, chat renderers, the main model) — narrowing that field's type
// would ripple through every consumer, so it stays the full union. See
// mcpEventsWorkspace below for a stub that only needs a role interface,
// because the function it exercises was narrowed instead.
type testWorkspace struct {
	workspace.Workspace
	cfg *config.Config
}

func (w *testWorkspace) Config() *config.Config {
	return w.cfg
}

// mcpEventsWorkspace is a [workspace.MCPController] stub — a fraction of
// testWorkspace's ~65-method surface — made possible because
// handleMCPPromptsEvent/handleMCPToolsEvent/handleMCPResourcesEvent take
// the narrow role interface directly instead of the full workspace.Workspace
// they used to require.
type mcpEventsWorkspace struct {
	workspace.MCPController
	promptsRefreshed   []string
	toolsRefreshed     []string
	resourcesRefreshed []string
}

func (w *mcpEventsWorkspace) MCPRefreshPrompts(_ context.Context, name string) {
	w.promptsRefreshed = append(w.promptsRefreshed, name)
}

func (w *mcpEventsWorkspace) RefreshMCPTools(_ context.Context, name string) {
	w.toolsRefreshed = append(w.toolsRefreshed, name)
}

func (w *mcpEventsWorkspace) MCPRefreshResources(_ context.Context, name string) {
	w.resourcesRefreshed = append(w.resourcesRefreshed, name)
}

// TestHandleMCPEventsUseNarrowWorkspaceInterface pins the role-interface
// narrowing of the three MCP event handlers: they used to take the full
// workspace.Workspace for a single method call each, forcing any test
// double to implement (or stub out) all ~65 methods. Now they take
// workspace.MCPController, so mcpEventsWorkspace only has to implement the
// handful of methods it actually exercises.
func TestHandleMCPEventsUseNarrowWorkspaceInterface(t *testing.T) {
	t.Parallel()

	ws := &mcpEventsWorkspace{}

	handleMCPPromptsEvent(ws, "server-a")()
	handleMCPToolsEvent(ws, "server-b")()
	handleMCPResourcesEvent(ws, "server-c")()

	require.Equal(t, []string{"server-a"}, ws.promptsRefreshed)
	require.Equal(t, []string{"server-b"}, ws.toolsRefreshed)
	require.Equal(t, []string{"server-c"}, ws.resourcesRefreshed)
}
