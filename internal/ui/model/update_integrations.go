package model

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
)

// integrationsState holds the memoized results of the external-integration
// loads this file drives: MCP client state (for the header badge and
// /mcp), the discovered skill states (for the skills status list), and the
// user-invocable custom commands and MCP prompts (for the commands
// palette).
//
// Embedded anonymously (by value) on UI so its fields keep promoting
// unchanged (m.mcpStates, ...); see widgets.go for why.
type integrationsState struct {
	mcpStates   map[string]workspace.MCPClientInfo
	skillStates []*skills.SkillState

	// mcpVersion / skillsVersion bump every time mcpStates / skillStates are
	// replaced. The sidebar cache (sidebar.go) keys off these instead of
	// diffing the slices/maps themselves, since both are always assigned
	// wholesale when they change.
	mcpVersion    int
	skillsVersion int

	customCommands []commands.CustomCommand
	mcpPrompts     []commands.MCPPrompt
}

// userCommandsLoadedMsg is sent when user commands are loaded.
type userCommandsLoadedMsg struct {
	Commands []commands.CustomCommand
}

// mcpPromptsLoadedMsg is sent when mcp prompts are loaded.
type mcpPromptsLoadedMsg struct {
	Prompts []commands.MCPPrompt
}

// mcpStateChangedMsg is sent when there is a change in MCP client states.
type mcpStateChangedMsg struct {
	states map[string]workspace.MCPClientInfo
}

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
		m.mcpVersion++
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
		// Refresh the memoized LSP state off-thread: LSPGetStates is treated
		// as IO and diagnostics events can arrive per edited file.
		if cmd := m.requestLSPRefresh(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[skills.Event]:
		m.skillStates = msg.Payload.States
		m.skillsVersion++
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
				m.loadMCPromptsCmd(),
			))
			return cmds, true
		case workspace.MCPEventPromptsListChanged:
			cmds = append(cmds, handleMCPPromptsEvent(m.com.Context(), m.com.Workspace, msg.Payload.Name))
			return cmds, true
		case workspace.MCPEventToolsListChanged:
			cmds = append(cmds, handleMCPToolsEvent(m.com.Context(), m.com.Workspace, msg.Payload.Name))
			return cmds, true
		case workspace.MCPEventResourcesListChanged:
			cmds = append(cmds, handleMCPResourcesEvent(m.com.Context(), m.com.Workspace, msg.Payload.Name))
			return cmds, true
		}
	}
	return cmds, false
}

// loadMCPromptsCmd loads the MCP prompts asynchronously. It snapshots the
// workspace and context before returning the closure: callers pass the
// result directly as a tea.Cmd, which runs off the Update goroutine.
func (m *UI) loadMCPromptsCmd() tea.Cmd {
	ws := m.com.Workspace
	ctx := m.com.Context()
	return func() tea.Msg {
		prompts, err := ws.ListMCPPrompts(ctx)
		if err != nil {
			slog.Error("Failed to load MCP prompts", "error", err)
		}
		if prompts == nil {
			// flag them as loaded even if there is none or an error
			prompts = []commands.MCPPrompt{}
		}
		return mcpPromptsLoadedMsg{Prompts: prompts}
	}
}
