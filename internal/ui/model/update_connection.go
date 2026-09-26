package model

import (
	tea "charm.land/bubbletea/v2"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/workspace"
)

// connectionState tracks whether this UI's remote workspace event stream is
// currently healthy, for the persistent header indicator (CLIENT-SERVER.md,
// PR 1.4c). An in-process Workspace never publishes workspace.ConnectionEvent
// (see that type's own doc comment), so lost stays false for the whole life
// of such a UI, and drawHeader's indicator never appears.
type connectionState struct {
	lost bool
}

// updateConnection handles pubsub.Event[workspace.ConnectionEvent]. Lost
// arms the persistent header indicator; it is not TTL-based like
// Status.ShowInfo, since the outage may well outlast any timer.
// Recovered/Resync both clear it. Resync additionally reloads everything
// this UI holds that live events would otherwise have kept current: the
// gRPC client's own resync (grpcws.Client.resync) refills its cache and
// re-delivers pending permission/question requests as ordinary request
// events, which the existing per-ID dedupe in openPermissionsDialog/
// openBatchFormDialog already protects against a second dialog — but
// nothing replays the message/session/delegation/LSP/MCP events this UI
// applied incrementally while disconnected, so those have to be pulled
// fresh instead of trusted to have kept up.
//
// Reached identically by an embedded thread UI's own Update: threadEventMsg
// forwards its inner message unwrapped into the child *UI's Update, which
// looks this type up in the same updateGroups table (see root.go's
// handleThreadAttached and Root.Update's threadEventMsg case), so a
// thread's own SubscribeWith pump gets the same indicator/reload behavior
// scoped to that UI's own state without any extra wiring here.
func (m *UI) updateConnection(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	evt, ok := msg.(pubsub.Event[workspace.ConnectionEvent])
	if !ok {
		return cmds, false
	}
	switch evt.Payload.State {
	case workspace.ConnectionLost:
		m.conn.lost = true
	case workspace.ConnectionRecovered:
		m.conn.lost = false
	case workspace.ConnectionResync:
		m.conn.lost = false
		cmds = append(cmds, m.resyncCmds()...)
	}
	return cmds, false
}

// resyncCmds reloads the current session and its messages, the sessions
// dialog if it's open, the delegations/task caches, and the memoized
// LSP/MCP state, following a Resync. Every one of these is either a TTL
// cache that would otherwise serve stale data until its own backstop fires,
// or a dialog whose fetched snapshot has no other refresh trigger.
func (m *UI) resyncCmds() []tea.Cmd {
	var cmds []tea.Cmd

	if m.sess.current != nil {
		cmds = append(cmds, m.requestSessionLoad(m.sess.current.ID))
	}

	if m.dialog.ContainsDialog(dialog.SessionsID) {
		m.dialog.CloseDialog(dialog.SessionsID)
		if cmd := m.openSessionsDialog(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	m.threadList.Invalidate()
	m.agentList.cache.Invalidate()

	if cmd := m.requestLSPRefresh(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.com != nil && m.com.Workspace != nil {
		cmds = append(cmds, m.handleStateChanged(), loadMCPromptsCmd(m.com, m))
	}

	return cmds
}
