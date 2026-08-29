package model

import (
	"testing"

	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// mcpPromptWorkspace answers GetMCPPrompt with an empty prompt so the
// load command resolves to nil rather than a sendMessageMsg — this test
// only cares about which dialog the trailing close targets, not message
// dispatch.
type mcpPromptWorkspace struct {
	*countingWorkspace
}

func (w *mcpPromptWorkspace) GetMCPPrompt(clientID, promptID string, args map[string]string) (string, error) {
	return "", nil
}

// TestRunMCPPromptClosesOnlyItsOwnDialog covers a regression: runMCPPrompt
// used to close whatever dialog happened to be in front when its
// GetMCPPrompt call finished, via an unconditional closeDialogMsg. If a
// different dialog (e.g. a permission prompt) got pushed to front while
// the call was in flight, that unconditional close swept it away instead
// of the dialog that actually started the MCP prompt. It must instead
// close the dialog that was in front when the call started, by ID,
// wherever that dialog now sits in the stack.
func TestRunMCPPromptClosesOnlyItsOwnDialog(t *testing.T) {
	t.Parallel()

	ws := &mcpPromptWorkspace{countingWorkspace: &countingWorkspace{ready: true}}
	m := newBusyUI(ws)

	// The dialog open when the MCP prompt call starts (e.g. the arguments
	// or commands dialog).
	m.dialog.OpenDialog(stubIDDialog{id: dialog.CommandsID})

	cmd := m.runMCPPrompt(m.com, "client", "prompt", nil)
	require.NotNil(t, cmd)

	// While GetMCPPrompt is "in flight" a different dialog (a permission
	// prompt) gets pushed to front, ahead of the one that started the
	// call.
	m.dialog.OpenDialog(stubIDDialog{id: dialog.PermissionsID})

	// Drive the sequence to completion exactly as the Bubble Tea runtime
	// would, routing every resulting message through Update.
	runCmdTree(m, cmd, nil)

	require.False(t, m.dialog.ContainsDialog(dialog.CommandsID),
		"the dialog that started the MCP prompt call must be closed")
	require.True(t, m.dialog.ContainsDialog(dialog.PermissionsID),
		"a dialog pushed in front while the call was in flight must survive")
}
