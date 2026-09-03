package dialog

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/util"
)

const SessionsID = "session"

type sessionsMode uint8

const (
	sessionsModeNormal sessionsMode = iota
	sessionsModeDeleting
	sessionsModeUpdating
)

// Session composes selectDialog's filter input, list, navigation, help and
// centered layout with the session-specific delete and rename state machine.
type Session struct {
	*selectDialog
	com          *common.Common
	sessions     []session.Session
	sessionsMode sessionsMode
	sessionKeys  struct{ Delete, Rename, ConfirmRename, CancelRename, ConfirmDelete, CancelDelete key.Binding }
}

var _ Dialog = (*Session)(nil)

func NewSessions(com *common.Common, sessions []session.Session, selectedSessionID string) *Session {
	s := &Session{com: com, sessions: sessions}
	sd, err := newSelectDialog(com, selectDialogConfig{id: SessionsID, title: "Sessions", maxWidth: defaultDialogMaxWidth, inputPlaceholder: "Enter session name", selectKeys: []string{"enter", "tab", "ctrl+y"}, dynamicHeight: true, maxHeight: defaultDialogHeight, buildItems: s.sessionItems, onSelect: func(id string) Action {
		for _, session := range s.sessions {
			if session.ID == id {
				return ActionSelectSession{Session: session}
			}
		}
		return nil
	}})
	if err != nil {
		panic(err)
	} // sessionItems is in-memory and cannot fail.
	s.selectDialog = sd
	for i, item := range s.list.FilteredItems() {
		if sessionItem, ok := item.(*SessionItem); ok && sessionItem.ID() == selectedSessionID {
			s.list.SetSelected(i)
			s.list.ScrollToSelected()
			break
		}
	}
	s.sessionKeys.Delete = key.NewBinding(key.WithKeys("ctrl+x"), key.WithHelp("ctrl+x", "delete"))
	s.sessionKeys.Rename = key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "rename"))
	s.sessionKeys.ConfirmRename = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm"))
	s.sessionKeys.CancelRename = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
	s.sessionKeys.ConfirmDelete = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "delete"))
	s.sessionKeys.CancelDelete = key.NewBinding(key.WithKeys("n", "esc"), key.WithHelp("n", "cancel"))
	return s
}
func (s *Session) ID() string { return SessionsID }
func (s *Session) sessionItems() ([]list.FilterableItem, int, error) {
	return sessionItems(s.com.Styles, s.sessionsMode, s.sessions...), 0, nil
}

// rebuild re-runs sessionItems after the underlying session list changed
// (rename, delete, pin) — not something the user typed a filter to
// trigger, so keep it.
func (s *Session) rebuild() { _ = s.replaceItems(s.sessionItems, true) }

func (s *Session) HandleMsg(msg tea.Msg) Action {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch s.sessionsMode {
	case sessionsModeDeleting:
		switch {
		case key.Matches(keyMsg, s.sessionKeys.ConfirmDelete):
			action := s.confirmDeleteSession()
			s.rebuild()
			return action
		case key.Matches(keyMsg, s.sessionKeys.CancelDelete):
			s.sessionsMode = sessionsModeNormal
			s.rebuild()
		}
		return nil
	case sessionsModeUpdating:
		switch {
		case key.Matches(keyMsg, s.sessionKeys.ConfirmRename):
			action := s.confirmRenameSession()
			s.rebuild()
			return action
		case key.Matches(keyMsg, s.sessionKeys.CancelRename):
			s.sessionsMode = sessionsModeNormal
			s.rebuild()
			return nil
		}
		if item := s.selectedSessionItem(); item != nil {
			// HandleInput returns a bare tea.Cmd, not a dialog.Action, so it
			// must be wrapped in ActionCmd before returning: an unwrapped
			// tea.Cmd falls into applyDialogAction's generic "unhandled
			// Action" default branch, which re-sends whatever it's handed
			// as a Bubble Tea *message* — a func value masquerading as a
			// tea.Msg — that Session.HandleMsg's own type switch can never
			// match, silently dropping the textinput's cmd instead of
			// running it.
			if cmd := item.HandleInput(keyMsg); cmd != nil {
				return ActionCmd{cmd}
			}
			return nil
		}
		return nil
	}
	switch {
	case key.Matches(keyMsg, s.sessionKeys.Rename):
		s.sessionsMode = sessionsModeUpdating
		s.rebuild()
		return nil
	case key.Matches(keyMsg, s.sessionKeys.Delete):
		if s.isCurrentSessionBusy() {
			return ActionCmd{Cmd: util.ReportWarn("Agent is busy, please wait...")}
		}
		s.sessionsMode = sessionsModeDeleting
		s.rebuild()
		return nil
	}
	return s.handleNavigation(keyMsg, func(item ListItem) Action {
		return ActionSelectSession{Session: item.(*SessionItem).Session}
	})
}

func (s *Session) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := s.com.Styles
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	layout := s.layout(area, height, true)
	width, innerWidth := layout.width, layout.innerWidth
	listHeight, listTotalHeight := layout.listHeight, layout.listTotalHeight
	applyInfoColumnVisibility(s.list.FilteredItems(), layout.listWidth, sessionInfoMaxPercent)
	start, end := s.list.VisibleItemIndices()
	if selected := s.list.Selected(); selected < start || selected > end {
		s.list.ScrollToSelected()
	}
	var cur *tea.Cursor
	rc := NewRenderContext(t, width)
	rc.Title = "Sessions"
	switch s.sessionsMode {
	case sessionsModeDeleting:
		rc.TitleStyle = t.Dialog.Sessions.DeletingTitle
		rc.TitleGradientFromColor = t.Dialog.Sessions.DeletingTitleGradientFromColor
		rc.TitleGradientToColor = t.Dialog.Sessions.DeletingTitleGradientToColor
		rc.ViewStyle = t.Dialog.Sessions.DeletingView
		rc.AddPart(t.Dialog.Sessions.DeletingMessage.Render("Delete this session?"))
	case sessionsModeUpdating:
		rc.TitleStyle = t.Dialog.Sessions.RenamingingTitle
		rc.TitleGradientFromColor = t.Dialog.Sessions.RenamingTitleGradientFromColor
		rc.TitleGradientToColor = t.Dialog.Sessions.RenamingTitleGradientToColor
		rc.ViewStyle = t.Dialog.Sessions.RenamingView
		message := t.Dialog.Sessions.RenamingingMessage.Render("Rename this session?")
		rc.AddPart(message)
		if item := s.selectedSessionItem(); item != nil {
			cur = item.Cursor()
			if cur != nil {
				inputStyle, titleStyle, dialogStyle := t.Dialog.InputPrompt, t.Dialog.Sessions.RenamingingTitle, t.Dialog.Sessions.RenamingView
				cur.X += inputStyle.GetBorderLeftSize() + inputStyle.GetMarginLeft() + inputStyle.GetPaddingLeft() + dialogStyle.GetBorderLeftSize() + dialogStyle.GetPaddingLeft() + dialogStyle.GetMarginLeft()
				cur.Y += titleStyle.GetVerticalFrameSize() + inputStyle.GetBorderTopSize() + inputStyle.GetMarginTop() + inputStyle.GetPaddingTop() + inputStyle.GetBorderBottomSize() + inputStyle.GetMarginBottom() + inputStyle.GetPaddingBottom() + dialogStyle.GetPaddingTop() + dialogStyle.GetBorderTopSize() + lipgloss.Height(message) - 1
				start, end := s.list.VisibleItemIndices()
				for selected := s.list.Selected(); start <= end && start != selected && selected > -1; start++ {
					cur.Y++
				}
			}
		}
	default:
		rc.AddPart(t.Dialog.InputPrompt.Render(s.input.View()))
		cur = s.Cursor()
	}
	listView := t.Dialog.List.Height(s.list.Height()).Render(s.list.Render())
	rc.AddPart(joinScrollbar(t, listView, listHeight, listTotalHeight, listHeight, s.list.Offset()))
	rc.Help = renderDialogHelp(t, &s.help, s, innerWidth)
	view := rc.Render()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

func (s *Session) selectedSessionItem() *SessionItem {
	if item := s.list.SelectedItem(); item != nil {
		return item.(*SessionItem)
	}
	return nil
}

func (s *Session) confirmDeleteSession() Action {
	sessionItem := s.selectedSessionItem()
	s.sessionsMode = sessionsModeNormal
	if sessionItem == nil {
		return nil
	}

	s.removeSession(sessionItem.ID())
	return ActionCmd{s.deleteSessionCmd(sessionItem.ID())}
}

func (s *Session) removeSession(id string) {
	var newSessions []session.Session
	for _, sess := range s.sessions {
		if sess.ID == id {
			continue
		}
		newSessions = append(newSessions, sess)
	}
	s.sessions = newSessions
}

func (s *Session) deleteSessionCmd(id string) tea.Cmd {
	return func() tea.Msg {
		err := s.com.Workspace.DeleteSession(s.com.Context(), id)
		if err != nil {
			return util.NewErrorMsg(err)
		}
		return nil
	}
}

func (s *Session) confirmRenameSession() Action {
	sessionItem := s.selectedSessionItem()
	s.sessionsMode = sessionsModeNormal
	if sessionItem == nil {
		return nil
	}

	newTitle := strings.TrimSpace(sessionItem.InputValue())
	if newTitle == "" {
		return nil
	}
	sess := sessionItem.Session
	sess.Title = newTitle
	s.updateSession(sess)
	return ActionCmd{s.renameSessionCmd(sess.ID, newTitle)}
}

func (s *Session) updateSession(session session.Session) {
	for existingID, sess := range s.sessions {
		if sess.ID == session.ID {
			s.sessions[existingID] = session
			break
		}
	}
}

// renameSessionCmd persists only the title through RenameSession, not the
// dialog's cached session row: that row is a ListSessions snapshot taken
// when the dialog opened, and the dialog never subscribes to session
// updates, so it can be arbitrarily stale by the time the user confirms a
// rename. Writing it back whole (SaveSession) would clobber cost, todos and
// summary_message_id changed by other writers (usage saves, the todo tool,
// auto-summarization) while the dialog sat open.
func (s *Session) renameSessionCmd(id, title string) tea.Cmd {
	return func() tea.Msg {
		if err := s.com.Workspace.RenameSession(s.com.Context(), id, title); err != nil {
			return util.NewErrorMsg(err)
		}
		return nil
	}
}

func (s *Session) isCurrentSessionBusy() bool {
	sessionItem := s.selectedSessionItem()
	if sessionItem == nil {
		return false
	}

	if s.com.Workspace == nil || !s.com.Workspace.AgentIsReady() {
		return false
	}

	return s.com.Workspace.AgentIsSessionBusy(sessionItem.ID())
}

// ShortHelp implements [help.KeyMap].
func (s *Session) ShortHelp() []key.Binding {
	switch s.sessionsMode {
	case sessionsModeDeleting:
		return []key.Binding{
			s.sessionKeys.ConfirmDelete,
			s.sessionKeys.CancelDelete,
		}
	case sessionsModeUpdating:
		return []key.Binding{
			s.sessionKeys.ConfirmRename,
			s.sessionKeys.CancelRename,
		}
	default:
		return []key.Binding{
			s.keyMap.UpDown,
			s.sessionKeys.Rename,
			s.sessionKeys.Delete,
			s.keyMap.Select,
			s.keyMap.Close,
		}
	}
}

// FullHelp implements [help.KeyMap].
func (s *Session) FullHelp() [][]key.Binding {
	m := [][]key.Binding{}
	slice := []key.Binding{
		s.keyMap.UpDown,
		s.sessionKeys.Rename,
		s.sessionKeys.Delete,
		s.keyMap.Select,
		s.keyMap.Close,
	}

	switch s.sessionsMode {
	case sessionsModeDeleting:
		slice = []key.Binding{
			s.sessionKeys.ConfirmDelete,
			s.sessionKeys.CancelDelete,
		}
	case sessionsModeUpdating:
		slice = []key.Binding{
			s.sessionKeys.ConfirmRename,
			s.sessionKeys.CancelRename,
		}
	}
	for i := 0; i < len(slice); i += 4 {
		end := min(i+4, len(slice))
		m = append(m, slice[i:end])
	}
	return m
}
