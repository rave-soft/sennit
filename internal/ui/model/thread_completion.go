package model

import (
	"fmt"
	"strings"
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
// the message store/the DB, so they don't survive a session reload or
// appear if the user is looking at a different session than the one that
// owns the thread. Fabricating a fake message.Message row here to force a
// persisted entry would be exactly the kind of workaround the "don't
// fabricate a fake persisted chat message" guidance rules out. A toast is
// the explicitly sanctioned fallback; the durable record remains the
// delegations dashboard, which already shows terminal status per row.

// isTerminalThreadStatus reports whether status is a known finished state.
// Unknown statuses deliberately remain neither active nor terminal, matching
// the domain and preventing an uncertain state from being presented as done.
//
// Idle is excluded: a thread created without a
// goal transitions pending -> idle, and a reactivated one goes
// completed -> idle. Neither is work finishing, so neither should raise a
// "thread finished" toast.
func isTerminalThreadStatus(status string) bool {
	return proto.ThreadStatus(status).Terminal()
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
func (n *notifyState) notifyThreadCompletion(t proto.Thread) tea.Cmd {
	// Tasks ride the same event stream as threads (see updateThreads), and
	// they are not what this toast is for. A task is a subagent the
	// current turn started and is waiting on: its result comes back into
	// the transcript as the delegation's report, so a toast on top of that
	// says nothing new — it just interrupts, once per subagent, in a turn
	// that may have started several. A thread is the opposite case: it
	// outlives the turn that started it, and the dock is the only place it
	// is otherwise visible, so a thread finishing is worth the interruption.
	//
	// An empty Kind is a thread: older servers sent no discriminator at
	// all, matching listcache.threadEventMatchesKind.
	if proto.ThreadKind(t.Kind) == proto.ThreadKindTask {
		return nil
	}
	if n.threadLastStatus == nil {
		n.threadLastStatus = make(map[string]string)
	}
	prev, known := n.threadLastStatus[t.ID]
	n.threadLastStatus[t.ID] = t.Status
	if !known || prev == t.Status || !isTerminalThreadStatus(t.Status) {
		return nil
	}
	// The transition has been reported; nothing further needs prev's
	// status. Pruning here (rather than only on pubsub.DeletedEvent, see
	// updateThreads) keeps the map from growing unbounded over a long
	// session full of threads that finish but are never deleted — a
	// repeat event for the same terminal status is still a no-op, since
	// it now reads as an unseen thread and !known short-circuits above.
	delete(n.threadLastStatus, t.ID)
	return threadCompletionToast(t)
}

// threadCompletionToast formats and reports the toast for one terminal
// transition, e.g. "thread fix-auth completed · 12m" (info) or "thread
// fix-auth failed" (warn) — matching threadBadge's success/warn/error
// status groupings (dashboard parity: a thread that reads as a green
// badge there reports as an info toast here, a red badge as a warn toast).
func threadCompletionToast(t proto.Thread) tea.Cmd {
	name := t.Name
	if name == "" {
		name = t.ID
	}
	switch proto.ThreadStatus(t.Status) {
	case proto.ThreadStatusCompleted:
		switch {
		case strings.Contains(t.Error, "uncommitted changes"):
			return util.ReportWarn(fmt.Sprintf("thread %s completed; kept because it has uncommitted changes", name))
		case strings.Contains(t.Error, "unique commits"):
			return util.ReportWarn(fmt.Sprintf("thread %s completed; kept because it has unique commits", name))
		case strings.Contains(t.Error, "could not be verified"):
			return util.ReportWarn(fmt.Sprintf("thread %s completed; cleanup safety could not be verified", name))
		default:
			return util.ReportInfo(fmt.Sprintf("thread %s completed; clean worktree removed%s", name, threadCompletionElapsedSuffix(t)))
		}
	case proto.ThreadStatusFailed:
		return util.ReportWarn(fmt.Sprintf("thread %s failed", name))
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
