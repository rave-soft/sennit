package model

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// Thread completion is surfaced as a toast (util.ReportInfo/ReportWarn),
// not a persisted chat-transcript entry. Investigated first: this
// codebase has no mechanism for injecting a non-model, system-authored
// entry into a session's *persisted* transcript — message.Message rows
// are written by the agent loop (domain/agent) or by an actual user
// input, and internal/ui/chat's synthetic-looking items (e.g. ShellItem
// for bang-mode results) are UI-list-only: constructed straight into
// m.chat from local UI state, never round-tripped through
// message.Service/the DB, so they don't survive a session reload or
// appear if the user is looking at a different session than the one that
// owns the thread. Fabricating a fake message.Message row here to force a
// persisted entry would be exactly the kind of workaround the "don't
// fabricate a fake persisted chat message" guidance rules out. A toast is
// the explicitly sanctioned fallback; the durable record remains the
// /threads dashboard, which already shows terminal status per thread.

// isTerminalThreadStatus reports whether status is a resting/finished
// state — anything outside the active set (pending, running, merging).
// Deliberately !Active rather than Terminal: for a toast, an unknown
// status from a newer build reads as "no longer running" too.
//
// Idle is the one non-active status excluded: a thread created without a
// goal transitions pending -> idle, and a reactivated one goes
// completed -> idle. Neither is work finishing, so neither should raise a
// "thread finished" toast.
func isTerminalThreadStatus(status string) bool {
	s := proto.ThreadStatus(status)
	return !s.Active() && s != proto.ThreadStatusIdle
}

// notifyThreadCompletion detects a thread's edge transition into a
// terminal status and returns a toast cmd for it, or nil. It tracks each
// thread's last-seen status in m.threadLastStatus so it fires exactly
// once per transition:
//   - a repeated event reporting the same status (or any non-terminal
//     status, e.g. pending -> running) is a no-op;
//   - the very first sighting of a thread (unknown to threadLastStatus
//     yet) never fires even if it's already terminal — e.g. the initial
//     threadsDock population on session load populating a thread that
//     finished before this UI ever attached to it. Only real transitions
//     observed live are worth interrupting the user for.
func (m *UI) notifyThreadCompletion(t proto.Thread) tea.Cmd {
	if m.threadLastStatus == nil {
		m.threadLastStatus = make(map[string]string)
	}
	prev, known := m.threadLastStatus[t.ID]
	m.threadLastStatus[t.ID] = t.Status
	if !known || prev == t.Status || !isTerminalThreadStatus(t.Status) {
		return nil
	}
	// The transition has been reported; nothing further needs prev's
	// status. Pruning here (rather than only on pubsub.DeletedEvent, see
	// updateThreads) keeps the map from growing unbounded over a long
	// session full of threads that finish but are never deleted — a
	// repeat event for the same terminal status is still a no-op, since
	// it now reads as an unseen thread and !known short-circuits above.
	delete(m.threadLastStatus, t.ID)
	return threadCompletionToast(t)
}

// threadCompletionToast formats and reports the toast for one terminal
// transition, e.g. "thread fix-auth merged · 12m" (info) or "thread
// fix-auth failed" (warn) — matching threadBadge's success/warn/error
// status groupings (dashboard parity: a thread that reads as a green
// badge there reports as an info toast here, a red badge as a warn toast).
func threadCompletionToast(t proto.Thread) tea.Cmd {
	name := t.Name
	if name == "" {
		name = t.ID
	}
	switch proto.ThreadStatus(t.Status) {
	case proto.ThreadStatusMerged:
		return util.ReportInfo(fmt.Sprintf("thread %s merged%s", name, threadCompletionElapsedSuffix(t)))
	case proto.ThreadStatusCompleted:
		return util.ReportInfo(fmt.Sprintf("thread %s completed%s", name, threadCompletionElapsedSuffix(t)))
	case proto.ThreadStatusFailed:
		return util.ReportWarn(fmt.Sprintf("thread %s failed", name))
	case proto.ThreadStatusConflict:
		return util.ReportWarn(fmt.Sprintf("thread %s has a merge conflict", name))
	case proto.ThreadStatusMergeBlocked:
		return util.ReportWarn(fmt.Sprintf("thread %s is merge-blocked", name))
	case proto.ThreadStatusInterrupted:
		return util.ReportWarn(fmt.Sprintf("thread %s was interrupted", name))
	case proto.ThreadStatusCancelled:
		return util.ReportWarn(fmt.Sprintf("thread %s was cancelled", name))
	default:
		return util.ReportInfo(fmt.Sprintf("thread %s finished (%s)", name, t.Status))
	}
}

// threadCompletionElapsedSuffix renders " · 12m"-style suffix from
// CreatedAt/CompletedAt, or "" if either is missing/inconsistent —
// matching threadDockStatusText' policy of omitting a misleading duration
// rather than guessing.
func threadCompletionElapsedSuffix(t proto.Thread) string {
	if t.CreatedAt <= 0 || t.CompletedAt <= 0 || t.CompletedAt < t.CreatedAt {
		return ""
	}
	return " · " + presentation.FormatElapsed(time.Duration(t.CompletedAt-t.CreatedAt)*time.Second)
}
