package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/completions"
	"github.com/rave-soft/sennit/internal/ui/dialog"
)

// isAuthTimeout reports whether an error indicates the OAuth flow was
// cancelled or timed out. The SDK wraps context errors inside its own
// messages (e.g. "authorization cancelled: context canceled"), so we
// check both the error chain and the message text.
func isAuthTimeout(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "authorization cancelled")
}

// authenticateMCP runs the OAuth flow for a named MCP server using the
// provided context. The dialog owns the context and cancels it if the
// user closes the dialog.
func authenticateMCP(com *common.Common, ctx context.Context, name string) tea.Cmd {
	ws := com.Workspace
	return func() tea.Msg {
		if err := ws.MCPAuthenticate(ctx, name); err != nil {
			if isAuthTimeout(err) {
				return dialog.ActionMCPAuthErrored{Name: name, Error: fmt.Errorf("authentication timed out")}
			}
			return dialog.ActionMCPAuthErrored{Name: name, Error: err}
		}
		return dialog.ActionMCPAuthComplete{Name: name}
	}
}

// loadMCPResourceCompletions fetches the MCP resource catalog through the
// workspace (never the mcp package directly — see internal/ui/AGENTS.md on
// layering) for the @-completion popup.
func loadMCPResourceCompletions(com *common.Common) []completions.ResourceCompletionValue {
	infos := com.Workspace.MCPResources()
	result := make([]completions.ResourceCompletionValue, len(infos))
	for i, info := range infos {
		result[i] = completions.ResourceCompletionValue{
			MCPName:  info.MCPName,
			URI:      info.URI,
			Title:    info.Title,
			MIMEType: info.MIMEType,
		}
	}
	return result
}

// openMCPAuthDialog opens the MCP authentication dialog if any servers
// are pending auth. If the dialog is already open, it brings it to the
// front instead.
func (w *widgets) openMCPAuthDialog(com *common.Common) tea.Cmd {
	pending := com.Workspace.MCPPendingAuth()
	if len(pending) == 0 {
		return nil
	}
	if w.dialog.ContainsDialog(dialog.MCPAuthID) {
		w.dialog.BringToFront(dialog.MCPAuthID)
		return nil
	}
	dlg, cmd := dialog.NewMCPAuth(com, pending, com.Workspace.MCPAuthURL)
	w.dialog.OpenDialog(dlg)
	return cmd
}

// checkPendingMCPAuth waits for MCP initialization to finish and then
// checks whether any OAuth MCPs need authentication. This runs as a
// Bubble Tea command so it doesn't block the UI.
func checkPendingMCPAuth(com *common.Common) tea.Cmd {
	parentCtx := com.Context()
	ws := com.Workspace
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()
		if err := ws.WaitForMCPInit(ctx); err != nil {
			return nil
		}
		return mcpStateChangedMsg{
			states: ws.MCPGetStates(),
		}
	}
}
