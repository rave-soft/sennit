package model

import (
	"context"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

func (w *widgets) runMCPPrompt(com *common.Common, owner *UI, clientID, promptID string, arguments map[string]string) tea.Cmd {
	ws := com.Workspace
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
			uiOwned: uiOwned{owner: owner},
			Content: prompt,
		}
	}

	// Snapshot which dialog is in front before the (potentially slow)
	// GetMCPPrompt round trip starts. If something else — a permission
	// prompt, most plausibly — gets pushed to front while it's in
	// flight, the close below must still target this dialog specifically
	// rather than whatever now sits on top.
	var frontID string
	if front := w.dialog.DialogLast(); front != nil {
		frontID = front.ID()
	}

	var cmds []tea.Cmd
	if cmd := w.dialog.StartLoading(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	cmds = append(cmds, load, func() tea.Msg {
		return closeDialogMsg{uiOwned: uiOwned{owner: owner}, id: frontID}
	})

	return tea.Sequence(cmds...)
}

func (m *UI) handleStateChanged() tea.Cmd {
	ws := m.com.Workspace
	ctx := m.com.Context()
	return updateAgentModelCmd(m, func() tea.Msg {
		if err := ws.UpdateAgentModel(ctx); err != nil {
			return util.NewErrorMsg(err)
		}
		return mcpStateChangedMsg{
			uiOwned: uiOwned{owner: m},
			states:  ws.MCPGetStates(),
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
func enableDockerMCPCmd(com *common.Common) tea.Cmd {
	ws := com.Workspace
	ctx := com.Context()
	return func() tea.Msg {
		if err := ws.EnableDockerMCP(ctx); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("Docker MCP enabled and started successfully")
	}
}

// disableDockerMCPCmd snapshots the workspace before returning the closure;
// see enableDockerMCPCmd.
func disableDockerMCPCmd(com *common.Common) tea.Cmd {
	ws := com.Workspace
	return func() tea.Msg {
		if err := ws.DisableDockerMCP(); err != nil {
			return util.ReportError(err)()
		}
		return util.NewInfoMsg("Docker MCP disabled successfully")
	}
}
