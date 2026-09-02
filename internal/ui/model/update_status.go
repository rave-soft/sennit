package model

import (
	"log/slog"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// cancelTimerExpiredMsg is sent when the cancel timer expires.
//
// uiOwned: dispatched by cancelTimerCmd. Routed by active screen instead,
// an expiry that lands while the dashboard is up never resets the
// cancellation confirmation, so the esc-to-cancel affordance is stuck
// reading "press again to confirm" for the rest of the session.
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
		m.cancellation.expire()
	case util.InfoMsg:
		if msg.Type == util.InfoTypeError {
			slog.Error("Error reported", "error", msg.Msg)
		}
		// util.ClearStatusMsg is defined outside model, so it cannot embed
		// uiOwned itself (model already imports util) — wrapped here via
		// ownCmd instead. Routed by active screen instead, a thread's own
		// status message got cleared by whichever UI happened to be on top
		// when its timer fired, not necessarily the one that showed it.
		cmds = append(cmds, ownCmd(m, m.status.ShowInfo(msg)))
	case util.ClearStatusMsg:
		m.status.ClearInfoMsg(msg.Seq)
	}
	return cmds, false
}
