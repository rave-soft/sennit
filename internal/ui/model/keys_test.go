package model

import (
	"testing"

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
