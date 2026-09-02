package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// TestTUIConfigReadsToleratebareConfig drives every internal/ui/model site
// that reads a TUI option off Config() with a workspace whose Config()
// returns a bare &config.Config{} — no Options, no TUI, the shape
// internal/workspace's stubWorkspace test double returns and every
// hand-built test Config shares. Before config.Config grew ThemeID/
// SpinnerMode-shaped accessors for these fields (CompletionsLimits,
// DiffMode, Scrollbar, Keybindings, CompactMode, TransparentEnabled), some
// of these sites guarded against nil and some did not; this proves none of
// them panics now that they all go through the accessors.
func TestTUIConfigReadsTolerateBareConfig(t *testing.T) {
	t.Parallel()

	// bareConfigWorkspace reuses cmdDrivingWorkspace (already a full
	// workspace.Workspace implementation, exercised through New() by the
	// command-driving golden tests) but overrides Config() to return a
	// bare struct instead of cmdDrivingWorkspace's own Options-populated
	// one.
	ws := &bareConfigWorkspace{cmdDrivingWorkspace: &cmdDrivingWorkspace{agentReady: true}}

	// ui.go's New(): Scrollbar (chatlist wiring), Keybindings
	// (configuredKeyMap), and CompactMode (initial layout state).
	var u *UI
	require.NotPanics(t, func() {
		u = New(common.DefaultCommon(context.Background(), ws), "", false, withGOOS("linux"))
	})
	u.dialog = dialog.NewOverlay()

	// dialogs.go's openPermissionsDialog(): DiffMode.
	require.NotPanics(t, func() {
		u.openPermissionsDialog(permission.PermissionRequest{ID: "perm-1", ToolCallID: "tc-1", ToolName: "bash"})
	})
	require.True(t, u.dialog.ContainsDialog(dialog.PermissionsID))
	u.dialog.CloseDialog(dialog.PermissionsID)

	// keypress.go's "@" completion trigger: CompletionsLimits.
	u.state = uiChat
	u.focus = uiFocusEditor
	require.NotPanics(t, func() {
		u.handleKeyPressMsg(tea.KeyPressMsg{Text: "@", Code: '@'})
	})
	require.True(t, u.editor.completions.open)
}

// bareConfigWorkspace overrides cmdDrivingWorkspace's Config() to return a
// bare &config.Config{} (no Options, no TUI).
type bareConfigWorkspace struct {
	*cmdDrivingWorkspace
}

// Providers is non-nil only so New()'s unrelated IsConfigured() check has a
// map to range over — Options (and so Options.TUI) stays nil, which is
// what this test actually exercises.
func (w *bareConfigWorkspace) Config() *config.Config {
	return &config.Config{Providers: csync.NewMap[string, config.ProviderConfig]()}
}
