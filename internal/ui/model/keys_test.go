package model

import (
	"context"
	"testing"

	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/stretchr/testify/require"
)

func TestConfiguredKeyMapLinuxDefaults(t *testing.T) {
	t.Parallel()

	km := configuredKeyMap("linux", nil)
	require.Equal(t, []string{"ctrl+p"}, km.Commands.Keys())
	require.Equal(t, []string{"ctrl+down", "alt+down"}, km.Chat.EnterChildSession.Keys())
}

func TestConfiguredKeyMapDarwinUsesSuperDefaults(t *testing.T) {
	t.Parallel()

	km := configuredKeyMap("darwin", nil)
	require.Equal(t, []string{"super+p"}, km.Commands.Keys())
	require.Equal(t, []string{"super+down", "alt+down"}, km.Chat.EnterChildSession.Keys())
	require.Equal(t, []string{"super+v"}, km.Editor.PasteImage.Keys())
	require.Equal(t, "super+↓", km.Chat.EnterChildSession.Help().Key)
}

// TestConfiguredKeyMapDarwinKeepsTerminalConventions asserts that the
// darwin ctrl+->super+ rewrite leaves "quit" (ctrl+c) and "suspend"
// (ctrl+z) alone, since those are terminal interrupt/suspend conventions
// a macOS terminal user still relies on, while an ordinary binding still
// gets rewritten to its super+ form.
func TestConfiguredKeyMapDarwinKeepsTerminalConventions(t *testing.T) {
	t.Parallel()

	km := configuredKeyMap("darwin", nil)
	require.Equal(t, []string{"ctrl+c"}, km.Quit.Keys())
	require.Equal(t, "ctrl+c", km.Quit.Help().Key)
	require.Equal(t, []string{"ctrl+z"}, km.Suspend.Keys())
	require.Equal(t, "ctrl+z", km.Suspend.Help().Key)
	require.Equal(t, []string{"super+p"}, km.Commands.Keys())
}

func TestConfiguredKeyMapOverridesAllGroups(t *testing.T) {
	t.Parallel()

	km := configuredKeyMap("darwin", map[string][]string{
		"commands":                      {"ctrl+x", "alt+x"},
		"editor.newline":                {"alt+enter"},
		"chat.exit_child_session":       {"super+left"},
		"editor.attachment_delete_mode": {"alt+r"},
		"initialize.yes":                {"enter"},
	})

	require.Equal(t, []string{"ctrl+x", "alt+x"}, km.Commands.Keys())
	require.Equal(t, []string{"alt+enter"}, km.Editor.Newline.Keys())
	require.Equal(t, []string{"super+left"}, km.Chat.ExitChildSession.Keys())
	require.Equal(t, "super+←", km.Chat.ExitChildSession.Help().Key)
	require.Equal(t, "alt+r+{i}", km.Editor.AttachmentDeleteMode.Help().Key)
	require.Equal(t, "alt+r+r", km.Editor.DeleteAllAttachments.Help().Key)
	require.Equal(t, []string{"enter"}, km.Initialize.Yes.Keys())
}

func TestConfiguredKeyMapIgnoresUnknownAndEmptyOverrides(t *testing.T) {
	t.Parallel()

	km := configuredKeyMap("linux", map[string][]string{
		"unknown":  {"ctrl+x"},
		"commands": nil,
	})
	require.Equal(t, []string{"ctrl+p"}, km.Commands.Keys())
}

// TestNewRespectsWithGOOS pins the seam New() -> configuredKeyMap runs
// through: withGOOS must control the keyMap New() builds (and the shortcut
// baked into the attachments component from it), the same way passing
// "darwin"/"linux" directly to configuredKeyMap does above. Golden and
// keybinding-sensitive tests rely on this to stay host-independent — see
// newCmdDrivenGoldenUI and newTestRoot.
func TestNewRespectsWithGOOS(t *testing.T) {
	t.Parallel()

	ws := &rootTestWorkspace{}
	com := common.DefaultCommon(context.Background(), ws)

	darwinUI := New(com, "", false, withGOOS("darwin"))
	require.Equal(t, []string{"super+p"}, darwinUI.keyMap.Commands.Keys())

	linuxUI := New(com, "", false, withGOOS("linux"))
	require.Equal(t, []string{"ctrl+p"}, linuxUI.keyMap.Commands.Keys())
}
