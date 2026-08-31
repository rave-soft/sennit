package model

import (
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// cancelTimerExpiredMsg is sent when the cancel timer expires.
//
// uiOwned: dispatched by cancelTimerCmd. Routed by active screen instead,
// an expiry that lands while the dashboard is up never clears isCanceling,
// so the esc-to-cancel affordance is stuck reading "press again to
// confirm" for the rest of the session.
type cancelTimerExpiredMsg struct{ uiOwned }

// updateStatus handles the status-line branches of UI.Update: info/clear
// messages, server notices, update-available and connection notices, agent
// notifications, and the cancel-confirmation timer. It is called from
// Update's message-type switch and shares that switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateStatus(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case pubsub.Event[workspace.AgentNotification]:
		if cmd := m.handleAgentNotification(msg.Payload); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case cancelTimerExpiredMsg:
		m.isCanceling = false
	case util.InfoMsg:
		if msg.Type == util.InfoTypeError {
			slog.Error("Error reported", "error", msg.Msg)
		}
		cmds = append(cmds, m.status.ShowInfo(msg))
	case workspace.UpdateAvailableMsg:
		text := fmt.Sprintf("Sennit update available: v%s → v%s.", msg.CurrentVersion, msg.LatestVersion)
		if msg.IsDevelopment {
			text = fmt.Sprintf("This is a development version of Sennit. The latest version is v%s.", msg.LatestVersion)
		}
		cmds = append(cmds, m.status.ShowInfo(util.InfoMsg{
			Type: util.InfoTypeUpdate,
			Msg:  text,
			TTL:  10 * time.Second,
		}))
	case util.ClearStatusMsg:
		m.status.ClearInfoMsg(msg.Seq)
	}
	return cmds, false
}
