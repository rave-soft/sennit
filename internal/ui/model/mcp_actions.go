package model

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

func (m *UI) runMCPPrompt(clientID, promptID string, arguments map[string]string) tea.Cmd {
	load := func() tea.Msg {
		prompt, err := m.com.Workspace.GetMCPPrompt(clientID, promptID, arguments)
		if err != nil {
			// TODO: make this better
			return util.ReportError(err)()
		}

		if prompt == "" {
			return nil
		}
		return sendMessageMsg{
			Content: prompt,
		}
	}

	var cmds []tea.Cmd
	if cmd := m.dialog.StartLoading(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, load, func() tea.Msg {
		return closeDialogMsg{}
	})

	return tea.Sequence(cmds...)
}

func (m *UI) handleStateChanged() tea.Cmd {
	return m.updateAgentModelCmd(func() tea.Msg {
		if err := m.com.Workspace.UpdateAgentModel(m.com.Context()); err != nil {
			return util.NewErrorMsg(err)
		}
		return mcpStateChangedMsg{
			states: m.com.Workspace.MCPGetStates(),
		}
	})
}

func handleMCPPromptsEvent(ctx context.Context, ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.MCPRefreshPrompts(ctx, name)
		return nil
	}
}

func handleMCPToolsEvent(ctx context.Context, ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.RefreshMCPTools(ctx, name)
		return nil
	}
}

func handleMCPResourcesEvent(ctx context.Context, ws workspace.MCPController, name string) tea.Cmd {
	return func() tea.Msg {
		ws.MCPRefreshResources(ctx, name)
		return nil
	}
}

func (m *UI) enableDockerMCP() tea.Msg {
	ctx := m.com.Context()
	if err := m.com.Workspace.EnableDockerMCP(ctx); err != nil {
		return util.ReportError(err)()
	}

	return util.NewInfoMsg("Docker MCP enabled and started successfully")
}

func (m *UI) disableDockerMCP() tea.Msg {
	if err := m.com.Workspace.DisableDockerMCP(); err != nil {
		return util.ReportError(err)()
	}

	return util.NewInfoMsg("Docker MCP disabled successfully")
}
