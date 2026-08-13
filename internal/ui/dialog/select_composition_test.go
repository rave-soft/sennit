package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

type busySessionWorkspace struct{ workspace.Workspace }

func (busySessionWorkspace) AgentIsReady() bool             { return true }
func (busySessionWorkspace) AgentIsSessionBusy(string) bool { return true }

type selectCompositionItem struct {
	list.BaseItem
	id string
}

func (i *selectCompositionItem) Filter() string          { return i.id }
func (i *selectCompositionItem) ID() string              { return i.id }
func (i *selectCompositionItem) Render(width int) string { return i.id }

func keyPress(key string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: []rune(key)[0], Text: key} }

func commandSelectionID(c *Commands) string {
	item, _ := c.list.SelectedItem().(*CommandItem)
	if item == nil {
		return ""
	}
	return item.ID()
}

func TestSelectDialogComposition_FilterNavigationAndNarrowLayout(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	d, err := newSelectDialog(com, selectDialogConfig{
		id: "test", title: "Test", maxWidth: 50,
		buildItems: func() ([]list.FilterableItem, int, error) {
			return []list.FilterableItem{
				&selectCompositionItem{BaseItem: list.NewBaseItem(), id: "alpha"},
				&selectCompositionItem{BaseItem: list.NewBaseItem(), id: "beta"},
			}, 0, nil
		},
		onSelect: func(id string) Action { return ActionSelectNotificationStyle{Style: id} },
	})
	require.NoError(t, err)

	d.HandleMsg(keyPress("b"))
	require.Equal(t, "b", d.input.Value())
	require.Len(t, d.list.FilteredItems(), 1)
	require.Equal(t, "beta", d.selectedID())

	d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "beta", d.selectedID(), "single filtered row wraps to itself")
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, ActionSelectNotificationStyle{Style: "beta"}, action)

	d.Resize(uv.Rectangle{Max: uv.Position{X: 8, Y: 4}})
	require.GreaterOrEqual(t, d.Width(), 0)
	require.GreaterOrEqual(t, d.InnerWidth(), 0)
}

func TestCommandsComposition_CategoryAndAsyncRebuildPreserveSelection(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	custom := []commands.CustomCommand{{ID: "custom", Name: "custom", Content: "run"}}
	c, err := NewCommands(com, "", false, false, false, custom, nil)
	require.NoError(t, err)

	c.list.SetSelected(1) // switch_session has a stable ID across the async rebuild.
	selected := commandSelectionID(c)
	require.NotEmpty(t, selected)
	c.HandleMsg(dockerMCPAvailabilityCheckedMsg{available: false})
	require.Equal(t, selected, commandSelectionID(c))

	c.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, UserCommands, c.selected)
	require.Len(t, c.list.FilteredItems(), 1)
	c.HandleMsg(tea.KeyPressMsg{Text: "shift+tab"})
	require.Equal(t, SystemCommands, c.selected)
}

func TestSessionNormalModeTabSelectsCurrentSession(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	sessions := []session.Session{{ID: "one", Title: "One"}, {ID: "two", Title: "Two"}}
	s := NewSessions(com, sessions, "two")

	action := s.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	require.Equal(t, ActionSelectSession{Session: sessions[1]}, action)
	require.Empty(t, s.input.Value(), "tab must not be forwarded to the filter input")
}

func TestCommandsSelectionTakesPriorityOverConflictingShortcut(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	c, err := NewCommands(com, "", false, false, false, nil, nil)
	require.NoError(t, err)
	c.list.SetSelected(1) // switch_session; ctrl+y also belongs to toggle_yolo.

	action := c.HandleMsg(tea.KeyPressMsg{Text: "ctrl+y"})
	require.Equal(t, ActionOpenDialog{DialogID: SessionsID}, action)
}

func TestCommandsShortcutTakesPriorityOverSharedNavigation(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	c, err := NewCommands(com, "", false, false, false, nil, nil)
	require.NoError(t, err)
	c.list.SetSelected(1)

	action := c.HandleMsg(tea.KeyPressMsg{Text: "ctrl+n"})
	require.Equal(t, ActionNewSession{}, action)
	require.Equal(t, 1, c.list.Selected(), "ctrl+n must not use shared next navigation")
}

func TestSessionComposition_RenameDeleteAndCancelTransitions(t *testing.T) {
	com := newCommandsNamesTestCommon(t)
	s := NewSessions(com, []session.Session{{ID: "one", Title: "One"}, {ID: "two", Title: "Two"}}, "two")
	s.com.Workspace = nil // Delete's busy guard treats a detached workspace as idle.
	require.Equal(t, "two", s.selectedID())

	s.HandleMsg(tea.KeyPressMsg{Text: "ctrl+r"})
	require.Equal(t, sessionsModeUpdating, s.sessionsMode)
	require.NotNil(t, s.selectedSessionItem())
	s.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, sessionsModeNormal, s.sessionsMode, "escape cancels rename, not the dialog")

	s.com.Workspace = busySessionWorkspace{}
	busy := s.HandleMsg(tea.KeyPressMsg{Text: "ctrl+x"})
	_, warned := busy.(ActionCmd)
	require.True(t, warned, "busy sessions cannot enter delete confirmation")
	require.Equal(t, sessionsModeNormal, s.sessionsMode)
	s.com.Workspace = nil

	s.HandleMsg(tea.KeyPressMsg{Text: "ctrl+x"})
	require.Equal(t, sessionsModeDeleting, s.sessionsMode)
	s.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Equal(t, sessionsModeNormal, s.sessionsMode, "escape cancels delete, not the dialog")

	s.HandleMsg(tea.KeyPressMsg{Text: "ctrl+x"})
	action := s.HandleMsg(keyPress("y"))
	require.Equal(t, sessionsModeNormal, s.sessionsMode)
	require.Len(t, s.sessions, 1)
	_, ok := action.(ActionCmd)
	require.True(t, ok, "confirmed deletion remains asynchronous")
}
