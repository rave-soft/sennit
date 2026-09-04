package model

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/ui/chat"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/notification"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// notifyState holds desktop-notification state: which backend is wired up,
// whether the terminal window currently has focus (notifications are
// suppressed while it does), and the per-thread last-seen status used to
// detect the edge transition into a terminal state (see
// notifyThreadCompletion in thread_completion.go).
//
// Embedded anonymously (by value) on UI so its fields keep promoting
// unchanged (m.notifyBackend, ...); see widgets.go for why.
type notifyState struct {
	notifyBackend       notification.Backend
	notifyWindowFocused bool

	// threadLastStatus tracks each thread's last-seen status, so
	// notifyThreadCompletion (thread_completion.go) can detect the exact
	// edge transition into a terminal state and toast it exactly once.
	threadLastStatus map[string]string
}

// sendNotification returns a command that sends a notification if allowed by policy.
func (m *UI) sendNotification(n notification.Notification) tea.Cmd {
	if !m.shouldSendNotification() {
		return nil
	}
	backend := m.notifyBackend
	return tea.Sequence(backend.Send(n), func() tea.Msg { return notificationSentMsg{uiOwned{owner: m}} })
}

// maxNotificationBodyLen caps notification body text so OS notification
// centers don't clip or wrap it awkwardly. Long session titles and error
// messages get truncated with an ellipsis.
const maxNotificationBodyLen = 120

// notificationTitle returns the desktop notification title. Appending the
// project directory name lets a user running Sennit in several workspaces
// at once tell which one a notification came from; falls back to plain
// "Sennit" when the working directory is unknown or root.
func notificationTitle(workingDir string) string {
	name := filepath.Base(filepath.Clean(workingDir))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return brand.Name
	}
	return "Sennit — " + name
}

// notificationBodyTaskFinished formats the body for an agent-turn-completed
// notification.
func notificationBodyTaskFinished(sessionTitle string) string {
	if sessionTitle == "" {
		return "Task finished"
	}
	return "Task finished: " + ansi.Truncate(sessionTitle, maxNotificationBodyLen, "…")
}

// notificationBodyTaskFailed formats the body for an agent-turn-errored
// notification.
func notificationBodyTaskFailed(errMessage string) string {
	errMessage = strings.TrimSpace(errMessage)
	if errMessage == "" {
		return "Task failed"
	}
	return "Task failed: " + ansi.Truncate(errMessage, maxNotificationBodyLen, "…")
}

// notificationBodyPermission formats the body for a permission-request
// notification.
func notificationBodyPermission(toolName string) string {
	return "Permission needed: " + toolName
}

// notificationBodyQuestions formats the body for a question-request
// notification.
func notificationBodyQuestions(count int) string {
	if count == 1 {
		return "Input needed: 1 question"
	}
	return fmt.Sprintf("Input needed: %d questions", count)
}

// selectNotificationBackend chooses the appropriate notification backend based
// on terminal capabilities, environment, and user configuration. This is a pure
// function that should be called once during initialization or when capabilities
// change.
func selectNotificationBackend(caps common.Capabilities, cfg *config.Config) notification.Backend {
	if cfg != nil && cfg.Options != nil && cfg.Options.Notifications != "" {
		switch cfg.Options.Notifications {
		case "native":
			if !notification.NativeSupported {
				slog.Debug("Native notifications unavailable on this platform; using OSC backend", "osc99_supported", caps.OSC99Notifications)
				return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
			}
			slog.Debug("Using native backend (user preference)")
			return notification.NewNativeBackend(notification.Icon)
		case "osc":
			slog.Debug("Using OSC backend (user preference)", "osc99_supported", caps.OSC99Notifications)
			return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
		case "bell":
			slog.Debug("Using bell backend (user preference)")
			return notification.NewBellBackend()
		case "disabled":
			slog.Debug("Notifications disabled (user preference)")
			return notification.NoopBackend{}
		case "auto":
			// Fall through to auto-detection below.
		default:
			slog.Warn("Unknown notification style, using auto", "style", cfg.Options.Notifications)
		}
	}

	_, isSSH := caps.Env.LookupEnv("SSH_TTY")

	// SSH sessions use terminal-based notifications (OSC 99 or 777).
	if isSSH {
		slog.Debug("Selected OSCBackend for SSH session", "osc99_supported", caps.OSC99Notifications)
		return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
	}

	// Local sessions: prefer OSC on macOS because the native backend (beeep)
	// uses terminal-notifier or AppleScript, which is slow and doesn't display
	// icons properly. Also prefer OSC where native notifications are unavailable
	// (illumos/solaris). OSC 99 provides a polished experience with icon support.
	if runtime.GOOS == "darwin" || !notification.NativeSupported {
		slog.Debug("Selected OSCBackend for local session", "osc99_supported", caps.OSC99Notifications, "native_supported", notification.NativeSupported)
		return notification.NewOSCBackend(notification.Icon, caps.OSC99Notifications)
	}

	// Non-macOS local sessions use native OS notifications if focus events are supported.
	// Without focus events, we can't suppress notifications when focused, so
	// we disable them entirely to avoid spamming the user.
	if caps.ReportFocusEvents {
		slog.Debug("Selected NativeBackend for local session")
		return notification.NewNativeBackend(notification.Icon)
	}

	slog.Debug("Selected NoopBackend (focus events not supported)")
	return notification.NoopBackend{}
}

func (m *UI) updateNotificationBackend() {
	cfg := m.com.Config()
	m.notifyBackend = selectNotificationBackend(m.caps, cfg)
}

// shouldSendNotification returns true if notifications should be sent based on
// current state. Focus reporting must be supported, window must not be
// focused, and notifications must not be disabled in config.
func (m *UI) shouldSendNotification() bool {
	cfg := m.com.Config()
	if cfg != nil && cfg.Options != nil && cfg.Options.Notifications == "disabled" {
		return false
	}
	return m.caps.ReportFocusEvents && !m.notifyWindowFocused
}

// handleQuestionNotification dismisses an open question form when
// any client resolved the pending batch. Only one question can be
// pending at a time, so any notification means the current form
// is stale regardless of BatchID.
func (m *UI) handleQuestionNotification(_ question.Notification) {
	if _, ok := m.activeInline.(*dialog.QuestionForm); ok {
		m.activeInline = nil
		m.editor.textarea.Focus()
		m.updateLayoutAndSize()
	}
}

// handlePermissionNotification updates tool items when permission state changes.
func (w *widgets) handlePermissionNotification(notification permission.PermissionNotification) {
	if toolItem := w.chat.MessageItem(notification.ToolCallID); toolItem != nil {
		if permItem, ok := toolItem.(chat.ToolMessageItem); ok {
			if notification.Granted {
				permItem.SetStatus(chat.ToolStatusRunning)
			} else {
				permItem.SetStatus(chat.ToolStatusAwaitingPermission)
			}
		}
	}

	// If this notification reflects a final resolution (granted or denied),
	// dismiss any open permissions dialog whose tool call ID matches. This
	// covers the case where another client resolved the request remotely.
	if !notification.Granted && !notification.Denied {
		return
	}
	if d := w.dialog.Dialog(dialog.PermissionsID); d != nil {
		if perm, ok := d.(*dialog.Permissions); ok && perm.ToolCallID() == notification.ToolCallID {
			w.dialog.CloseDialog(dialog.PermissionsID)
		}
	}
}

// handleAgentNotification translates domain agent events into desktop
// notifications using the UI notification backend.
func (m *UI) handleAgentNotification(n workspace.AgentNotification) tea.Cmd {
	var cmds []tea.Cmd
	switch n.Type {
	case workspace.AgentNotificationFinished:
		common.StopTurn(n.SessionID)
		cmds = append(cmds, m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyTaskFinished(n.SessionTitle),
		}))
	case workspace.AgentNotificationError:
		// Terminal edge like TypeAgentFinished, but the turn ended with an
		// error rather than a normal completion — surface it too instead of
		// leaving the user to notice the failure on their own.
		common.StopTurn(n.SessionID)
		// Report in-app as well as through the desktop notification
		// below. The notification alone is not enough: sendNotification
		// suppresses it while the terminal window is focused, which is
		// exactly when the user is watching. A failure raised before
		// streaming began (provider readiness, model resolution) also
		// has no assistant message in the transcript carrying its
		// FinishReasonError, so without this the only visible sign is
		// the busy indicator switching off.
		//
		// Gated on the session the error belongs to, unlike StopTurn and
		// the cache invalidation below: two top-level sessions can be
		// busy at once, and a status-bar report is not attributed to a
		// session the way a chat message is — showing it unconditionally
		// put session A's failure in the status bar while the person was
		// looking at session B, telling them the wrong turn just failed.
		if m.sess.current != nil && n.SessionID == m.sess.current.ID {
			cmds = append(cmds, util.ReportError(errors.New(n.Message)))
		}
		cmds = append(cmds, m.sendNotification(notification.Notification{
			Title:   notificationTitle(m.com.Workspace.WorkingDir()),
			Message: notificationBodyTaskFailed(n.Message),
		}))
	case workspace.AgentNotificationReAuthenticate:
		return m.handleReAuthenticate(n.ProviderID)
	case workspace.AgentNotificationAWSSSOAuth:
		return m.handleAWSSSOAuth(m.com, n.AWSSOCommand, n.AWSSOURL)
	case workspace.AgentNotificationAWSSSOResult:
		return m.handleAWSSSOAuthResult(n.Message)
	case workspace.AgentNotificationAccountRotated:
		// Informational only, not a busy->idle edge: the turn keeps
		// running on the newly activated account, so there is nothing
		// here for the busy/queue caches below to re-probe.
		return util.ReportInfo(n.Message)
	case workspace.AgentNotificationAccountRotationExhausted:
		// Also informational: the original provider error (already
		// surfaced through the normal error path) is what actually
		// ends the turn, this just explains why rotation couldn't help.
		return util.ReportWarn(n.Message)
	case workspace.AgentNotificationQueueChanged:
		// Not a busy→idle edge (the session may still be busy, or may
		// never have been) - only the queue pill is stale, so refresh
		// just that instead of also re-probing busy state. This is the
		// same machinery the terminal cases below use, reused rather
		// than duplicated: it exists precisely so an enqueue/drain/
		// cancel/clear shows up immediately instead of waiting out the
		// TTL backstop.
		m.promptQueue.invalidate()
		return m.dispatchPromptQueueRefresh()
	default:
		return nil
	}
	// TypeAgentFinished / TypeAgentError are the busy→idle edge: the agent
	// clears its active request before publishing precisely so observers
	// can re-probe. Drop the memoized busy state and re-fetch it and the
	// prompt queue off-thread.
	m.wsCache.invalidateBusyCaches()
	m.promptQueue.invalidate()
	if cmd := m.dispatchBusyRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if cmd := m.dispatchPromptQueueRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}
