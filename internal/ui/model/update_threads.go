package model

import (
	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// updateThreads handles the thread-tracking branches of UI.Update: the
// thread pubsub event and the shared list's / dock's off-thread loads. It
// is called from Update's message-type switch and shares that switch's
// cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateThreads(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case pubsub.Event[proto.Thread]:
		// Root fans this to both screens (see root.go); the main screen
		// only cares about keeping the shared list (and the header badge
		// it feeds) current.
		m.threadList.applyEvent(msg)
		if msg.Type == pubsub.DeletedEvent {
			m.threadsDock.dropActivity(msg.Payload.ID)
			delete(m.threadLastStatus, msg.Payload.ID)
		}
		cmds = append(cmds, m.threadViewsRefreshCmds()...)
		// A thread's edge transition into a terminal status (merged,
		// failed, ...) gets a toast — see thread_completion.go for why a
		// toast rather than a persisted chat entry. Skipped for a deleted
		// thread: there is no meaningful "transition" left to report, and
		// calling it here would just re-insert the threadLastStatus entry
		// dropped above.
		if msg.Type != pubsub.DeletedEvent {
			if cmd := m.notifyThreadCompletion(msg.Payload); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
		// A thread starting or finishing changes whether the panel has
		// live work to animate.
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case threadsLoadedMsg:
		loadCmds, applied := m.threadList.applyLoaded(m.com, msg)
		cmds = append(cmds, loadCmds...)
		if applied {
			// The freshly listed threads may have added or retired
			// per-thread activity; see threadsDockState.activityGen.
			m.threadsDock.activityGen++
		}
		// The freshly listed threads may introduce (or retire) live work.
		if cmd := m.syncPanelSpinner(); cmd != nil {
			cmds = append(cmds, cmd)
		}
	case threadDockActivityLoadedMsg:
		m.threadsDock.applyThreadActivityLoaded(msg)
	}
	return cmds, false
}
