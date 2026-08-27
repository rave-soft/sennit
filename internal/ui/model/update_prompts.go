package model

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/notification"
)

// updatePrompts handles the permission and question prompt branches of
// UI.Update: permission requests/notifications, question requests/
// notifications, and dialog-close requests. It is called from Update's
// message-type switch and shares that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updatePrompts(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case closeDialogMsg:
		if msg.id != "" {
			m.dialog.CloseDialog(msg.id)
		} else {
			m.dialog.CloseFrontDialog()
		}

	case pubsub.Event[permission.PermissionRequest]:
		if cmd := m.openPermissionsDialog(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyPermission(msg.Payload.ToolName),
		}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[permission.PermissionNotification]:
		m.handlePermissionNotification(msg.Payload)
	case pubsub.Event[question.Request]:
		m.openBatchFormDialog(msg.Payload)
		if cmd := m.chat.ScrollToBottomAndAnimate(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyQuestions(len(msg.Payload.Questions)),
		}); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case pubsub.Event[question.Notification]:
		m.handleQuestionNotification(msg.Payload)
	}
	return cmds, false
}
