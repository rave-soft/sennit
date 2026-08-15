package model

import (
	"context"
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/skills"
	"github.com/rave-soft/braid/internal/ui/dialog"
	"github.com/rave-soft/braid/internal/workspace"
)

// updateIntegrations handles the LSP, custom-command, and MCP branches of
// UI.Update: LSP state refresh, custom-command and MCP-prompt loads, prompt
// history loads, LSP/skills/MCP pubsub events, and MCP auth dialog actions.
// It is called from Update's message-type switch and shares that switch's
// cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateIntegrations(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case lspStatesMsg:
		if cmd := m.applyLSPStates(msg); cmd != nil {
			cmds = append(cmds, cmd)
		}

	case userCommandsLoadedMsg:
		m.customCommands = msg.Commands
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetCustomCommands(m.customCommands)
		}

	case mcpStateChangedMsg:
		m.mcpStates = msg.states
		// Auto-open the MCP auth dialog if any servers need authentication.
		if cmd := m.openMCPAuthDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case mcpPromptsLoadedMsg:
		m.mcpPrompts = msg.Prompts
		dia := m.dialog.Dialog(dialog.CommandsID)
		if dia == nil {
			break
		}

		commands, ok := dia.(*dialog.Commands)
		if ok {
			commands.SetMCPPrompts(m.mcpPrompts)
		}

	case promptHistoryLoadedMsg:
		m.editor.promptHistory.messages = msg.messages
		m.editor.promptHistory.index = -1
		m.editor.promptHistory.draft = ""

	case pubsub.Event[workspace.LSPEvent]:
		// Refresh the memoized LSP state off-thread: LSPGetStates is a
		// synchronous HTTP round-trip in client/server mode and diagnostics
		// events can arrive per edited file.
		if cmd := m.requestLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[skills.Event]:
		m.skillStates = msg.Payload.States
	case dialog.ActionMCPAuthStarted:
		cmds = append(cmds, m.authenticateMCP(msg.Ctx, msg.Name))
	case dialog.ActionMCPAuthComplete, dialog.ActionMCPAuthErrored:
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	case pubsub.Event[workspace.MCPEvent]:
		switch msg.Payload.Type {
		case workspace.MCPEventStateChanged:
			cmds = append(cmds, tea.Batch(
				m.handleStateChanged(),
				m.loadMCPrompts,
			))
			return cmds, true
		case workspace.MCPEventPromptsListChanged:
			cmds = append(cmds, handleMCPPromptsEvent(m.com.Workspace, msg.Payload.Name))
			return cmds, true
		case workspace.MCPEventToolsListChanged:
			cmds = append(cmds, handleMCPToolsEvent(m.com.Workspace, msg.Payload.Name))
			return cmds, true
		case workspace.MCPEventResourcesListChanged:
			cmds = append(cmds, handleMCPResourcesEvent(m.com.Workspace, msg.Payload.Name))
			return cmds, true
		}
	}
	return cmds, false
}

// loadMCPrompts loads the MCP prompts asynchronously.
func (m *UI) loadMCPrompts() tea.Msg {
	prompts, err := m.com.Workspace.ListMCPPrompts(context.Background())
	if err != nil {
		slog.Error("Failed to load MCP prompts", "error", err)
	}
	if prompts == nil {
		// flag them as loaded even if there is none or an error
		prompts = []commands.MCPPrompt{}
	}
	return mcpPromptsLoadedMsg{Prompts: prompts}
}
