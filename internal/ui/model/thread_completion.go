package model

import (
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/rave-soft/braid/internal/ui/util"
)

// Thread completion is surfaced as a toast (util.ReportInfo/ReportWarn),
// not a persisted chat-transcript entry. Investigated first: this
// codebase has no mechanism for injecting a non-model, system-authored
// entry into a session's *persisted* transcript — message.Message rows
// are written by the agent loop (internal/agent) or by an actual user
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
// state — anything outside activeDockThreads' active set (pending,
// running, merging). Mirrors the status groupings threadBadge already
// uses for the dashboard's badge coloring.
func isTerminalThreadStatus(status string) bool {
	switch thread.Status(status) {
	case thread.StatusPending, thread.StatusRunning, thread.StatusMerging:
		return false
	default:
		return true
	}
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
	switch thread.Status(t.Status) {
	case thread.StatusMerged:
		return util.ReportInfo(fmt.Sprintf("thread %s merged%s", name, threadCompletionElapsedSuffix(t)))
	case thread.StatusCompleted:
		return util.ReportInfo(fmt.Sprintf("thread %s completed%s", name, threadCompletionElapsedSuffix(t)))
	case thread.StatusFailed:
		return util.ReportWarn(fmt.Sprintf("thread %s failed", name))
	case thread.StatusConflict:
		return util.ReportWarn(fmt.Sprintf("thread %s has a merge conflict", name))
	case thread.StatusMergeBlocked:
		return util.ReportWarn(fmt.Sprintf("thread %s is merge-blocked", name))
	case thread.StatusInterrupted:
		return util.ReportWarn(fmt.Sprintf("thread %s was interrupted", name))
	default:
		return util.ReportInfo(fmt.Sprintf("thread %s finished (%s)", name, t.Status))
	}
}

// threadCompletionElapsedSuffix renders " · 12m"-style suffix from
// CreatedAt/CompletedAt, or "" if either is missing/inconsistent —
// matching threadDockBlockLines' policy of omitting a misleading duration
// rather than guessing.
func threadCompletionElapsedSuffix(t proto.Thread) string {
	if t.CreatedAt <= 0 || t.CompletedAt <= 0 || t.CompletedAt < t.CreatedAt {
		return ""
	}
	return " · " + formatThreadElapsed(time.Duration(t.CompletedAt-t.CreatedAt)*time.Second)
}

// formatThreadElapsed renders a duration the way the toast wants it:
// "45s", "4m12s", "1h02m" — mirroring chat/agent.go's formatElapsed.
func formatThreadElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}
