package model

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

func (m *UI) runMCPPrompt(clientID, promptID string, arguments map[string]string) tea.Cmd {
	ws := m.com.Workspace
	load := func() tea.Msg {
		prompt, err := ws.GetMCPPrompt(clientID, promptID, arguments)
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
	ws := m.com.Workspace
	ctx := m.com.Context()
	return m.updateAgentModelCmd(func() tea.Msg {
		if err := ws.UpdateAgentModel(ctx); err != nil {
			return util.NewErrorMsg(err)
		}
		return mcpStateChangedMsg{
			states: ws.MCPGetStates(),
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

// enableDockerMCPCmd snapshots the workspace and context before returning
// the closure: callers pass the result directly as a tea.Cmd, so it must
// not read m off the Update goroutine when it runs.
func (m *UI) enableDockerMCPCmd() tea.Cmd {
	ws := m.com.Workspace
	ctx := m.com.Context()
	return func() tea.Msg {
		if err := ws.EnableDockerMCP(ctx); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("Docker MCP enabled and started successfully")
	}
}

// disableDockerMCPCmd snapshots the workspace before returning the closure;
// see enableDockerMCPCmd.
func (m *UI) disableDockerMCPCmd() tea.Cmd {
	ws := m.com.Workspace
	return func() tea.Msg {
		if err := ws.DisableDockerMCP(); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("Docker MCP disabled successfully")
	}
}
