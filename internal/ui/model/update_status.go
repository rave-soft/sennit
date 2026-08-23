package model

import (
	"fmt"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/util"
	"github.com/rave-soft/sennit/internal/workspace"
)

// cancelTimerExpiredMsg is sent when the cancel timer expires.
type cancelTimerExpiredMsg struct{}

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
		m.status.SetInfoMsg(msg)
		ttl := msg.TTL
		if ttl <= 0 {
			ttl = DefaultStatusTTL
		}
		cmds = append(cmds, clearInfoMsgCmd(ttl, m.nextStatusSeq()))
	case pubsub.Event[proto.ServerNotice]:
		// Notices from core code arrive as the transport-neutral
		// proto.ServerNotice so that code doesn't need to depend on
		// internal/ui; convert to util.InfoMsg here at the boundary.
		info := util.InfoMsg{
			Type: serverNoticeLevelToInfoType(msg.Payload.Level),
			Msg:  msg.Payload.Message,
		}
		m.status.SetInfoMsg(info)
		ttl := info.TTL
		if ttl <= 0 {
			ttl = DefaultStatusTTL
		}
		cmds = append(cmds, clearInfoMsgCmd(ttl, m.nextStatusSeq()))
	case workspace.UpdateAvailableMsg:
		text := fmt.Sprintf("Sennit update available: v%s → v%s.", msg.CurrentVersion, msg.LatestVersion)
		if msg.IsDevelopment {
			text = fmt.Sprintf("This is a development version of Sennit. The latest version is v%s.", msg.LatestVersion)
		}
		ttl := 10 * time.Second
		m.status.SetInfoMsg(util.InfoMsg{
			Type: util.InfoTypeUpdate,
			Msg:  text,
			TTL:  ttl,
		})
		cmds = append(cmds, clearInfoMsgCmd(ttl, m.nextStatusSeq()))
	case util.ClearStatusMsg:
		// Only the timer armed for the message currently on screen may
		// clear it; an older one has been superseded.
		if msg.Seq == m.statusSeq {
			m.status.ClearInfoMsg()
		}
	}
	return cmds, false
}

// serverNoticeLevelToInfoType maps the transport-neutral
// proto.ServerNoticeLevel to the UI's own status-line severity type.
func serverNoticeLevelToInfoType(level proto.ServerNoticeLevel) util.InfoType {
	switch level {
	case proto.ServerNoticeLevelWarn:
		return util.InfoTypeWarn
	case proto.ServerNoticeLevelError:
		return util.InfoTypeError
	default:
		return util.InfoTypeInfo
	}
}

// nextStatusSeq stamps the status-line message about to be shown and
// returns the stamp for its clear timer. See util.ClearStatusMsg.
func (m *UI) nextStatusSeq() int {
	m.statusSeq++
	return m.statusSeq
}
