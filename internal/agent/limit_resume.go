package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// maxLimitResumeWait bounds how far ahead a session may be parked waiting
// for a provider limit to lift. A weekly window is the longest wait any
// supported plan can honestly produce; anything beyond that is a clock
// skew or a header this code has misread, and parking a session on it for
// weeks would be worse than reporting the failure and stopping.
const maxLimitResumeWait = 8 * 24 * time.Hour

// maxResumeBackoff caps resumeBackoff's doubling.
const maxResumeBackoff = 15 * time.Minute

// limitResumes holds the pending "pick this session back up when the
// provider's window resets" timers, one per session.
//
// A session has at most one: a later limit replaces the earlier timer
// rather than adding to it, and any new turn for the session (a prompt the
// person sends, a cancel, the resume itself) drops it. Two timers for one
// session would mean two continuation turns racing for the same idle slot,
// where one of them is guaranteed to be redundant.
type limitResumes struct {
	mu     sync.Mutex
	timers map[string]*time.Timer
	// failures counts, per session, how many resumes in a row were
	// scheduled off a reset time that had already passed - see
	// noteFailure and resumeBackoff.
	failures map[string]int
}

func newLimitResumes() *limitResumes {
	return &limitResumes{timers: make(map[string]*time.Timer), failures: make(map[string]int)}
}

// Every method below tolerates a nil receiver: an agent assembled as a
// bare struct literal (which several tests and one-off agents do) has no
// resume state, and "no state" is the same answer as "nothing pending"
// for all of them.

// arm replaces sessionID's pending resume with timer, stopping whatever
// was there.
func (l *limitResumes) arm(sessionID string, timer *time.Timer) {
	if l == nil {
		timer.Stop()
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if old, ok := l.timers[sessionID]; ok {
		old.Stop()
	}
	l.timers[sessionID] = timer
}

// noteFailure records one more resume for sessionID that had no future
// reset to wait for, and returns the new count.
func (l *limitResumes) noteFailure(sessionID string) int {
	if l == nil {
		return 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = make(map[string]int)
	}
	l.failures[sessionID]++
	return l.failures[sessionID]
}

// clearFailures forgets sessionID's run of failed resumes. Called when a
// turn finally succeeds, and when a resume is scheduled off a reset that
// genuinely lies ahead.
func (l *limitResumes) clearFailures(sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, sessionID)
}

// disarm drops sessionID's pending resume, if any. Safe to call for a
// session that has none, and for one whose timer has already fired.
func (l *limitResumes) disarm(sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if timer, ok := l.timers[sessionID]; ok {
		timer.Stop()
		delete(l.timers, sessionID)
	}
}

// disarmAll drops every pending resume. Used by CancelAll.
func (l *limitResumes) disarmAll() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for sessionID, timer := range l.timers {
		timer.Stop()
		delete(l.timers, sessionID)
	}
}

// pending reports whether sessionID has a resume armed. For tests and for
// the status the UI asks about.
func (l *limitResumes) pending(sessionID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.timers[sessionID]
	return ok
}

// scheduleLimitResume parks sessionID until the provider limit that just
// ended its turn lifts, then picks the work back up on its own.
//
// It fires only for a *ProviderLimitError - the one failure whose end is a
// known moment rather than a guess. Every other error still ends the turn
// for good: a resume is only defensible when waiting is demonstrably the
// whole remedy.
//
// Sub-agent and non-interactive turns are left alone. A delegation's
// parent turn failed with them and is the thing that would have to be
// resumed, and a `sennit run` caller is a process waiting on one run's
// terminal event, not a session someone will come back to in three hours.
func (a *sessionAgent) scheduleLimitResume(ctx context.Context, call SessionAgentCall, err error) {
	var limit *ProviderLimitError
	if !errors.As(err, &limit) {
		return
	}
	if a.isSubAgent || call.NonInteractive || a.limitResumes == nil {
		return
	}
	wait, ok := limitResumeWait(limit.ResetsAt, time.Now())
	if !ok {
		slog.Warn("Usage-limit resume not scheduled: reset is implausibly far out",
			"session_id", call.SessionID, "provider", limit.Provider, "resets_at", limit.ResetsAt)
		return
	}
	sessionID := call.SessionID
	if wait > resumeGrace {
		a.limitResumes.clearFailures(sessionID)
	} else {
		// Nothing ahead to wait for: the reset quoted has already been
		// and gone, which means the figures behind it are stale (a
		// snapshot is only refreshed by a request). Retrying on the
		// grace alone turns that into a request every thirty seconds
		// for as long as the session is open, so each such resume in a
		// row waits longer than the last.
		attempt := a.limitResumes.noteFailure(sessionID)
		wait = resumeBackoff(attempt)
		slog.Warn("Usage-limit resume has no future reset to wait for, backing off",
			"session_id", sessionID, "provider", limit.Provider,
			"resets_at", limit.ResetsAt, "attempt", attempt, "wait", wait)
	}

	// The wait outlives the turn that scheduled it by hours, so it must
	// not hang off that turn's context; the coordinator's lifecycle
	// context is what stops it at shutdown. Agents built without one
	// (tests, one-off agents) keep the caller's context, which is the
	// same choice startContinuation makes.
	runCtx := ctx
	if a.continuationContext != nil {
		runCtx = a.continuationContext()
	}
	a.limitResumes.arm(sessionID, time.AfterFunc(wait, func() {
		a.resumeAfterLimit(runCtx, sessionID, limit)
	}))
	slog.Info("Usage-limit resume scheduled",
		"session_id", sessionID, "provider", limit.Provider,
		"resets_at", limit.ResetsAt, "wait", wait)

	if a.notify != nil {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:  sessionID,
			RunID:      call.RunID,
			Type:       notify.TypeUsageLimitWaiting,
			ProviderID: limit.Provider,
			Message: fmt.Sprintf("%s: waiting for the usage limit to reset, continuing at %s",
				limit.providerLabel(), time.Now().Add(wait).Format("15:04")),
		})
	}
}

// limitResumeWait is how long to park a session whose provider window
// resets at resetsAt, and whether to park it at all.
//
// The grace is always added, and is also the floor: a window that is
// already back (a snapshot read a moment after the reset, or two clocks
// that disagree) is still given it, so the resumed turn does not race the
// reset it is waiting on. A reset further out than any real plan window
// is refused rather than parked on - see maxLimitResumeWait.
func limitResumeWait(resetsAt, now time.Time) (time.Duration, bool) {
	wait := resetsAt.Sub(now) + resumeGrace
	if wait > maxLimitResumeWait {
		return 0, false
	}
	return max(wait, resumeGrace), true
}

// resumeBackoff is how long the attempt-th consecutive resume with no
// future reset to wait for holds off: the grace, doubling, capped. The cap
// keeps a session that is genuinely waiting on something checking back
// often enough to be useful without becoming a poll.
func resumeBackoff(attempt int) time.Duration {
	wait := resumeGrace
	for range max(attempt-1, 0) {
		wait *= 2
		if wait >= maxResumeBackoff {
			return maxResumeBackoff
		}
	}
	return wait
}

// resumeAfterLimit runs the parked session's next turn once the limit has
// lifted. It carries no prompt of its own: the session's own history is
// where the interrupted work is, and a continuation turn is exactly the
// entry point that resumes on history without persisting a fabricated
// user message (see continuationPromptPlaceholder).
//
// It gives up quietly when the session is busy again - someone came back
// before the reset did, and their turn is the one that should hold the
// session - and when the coordinator has shut down in the meantime.
func (a *sessionAgent) resumeAfterLimit(ctx context.Context, sessionID string, limit *ProviderLimitError) {
	a.limitResumes.disarm(sessionID)
	if ctx.Err() != nil {
		return
	}
	if a.IsSessionBusy(sessionID) {
		slog.Info("Usage-limit resume skipped: session is busy again", "session_id", sessionID)
		return
	}
	slog.Info("Usage-limit resume starting", "session_id", sessionID, "provider", limit.Provider)
	if a.notify != nil {
		a.notify.Publish(pubsub.CreatedEvent, notify.Notification{
			SessionID:  sessionID,
			Type:       notify.TypeUsageLimitResumed,
			ProviderID: limit.Provider,
			Message:    fmt.Sprintf("%s: the usage limit has reset, continuing", limit.providerLabel()),
		})
	}

	var err error
	if a.continuationRunner != nil {
		err = a.continuationRunner(ctx, sessionID)
	} else {
		_, err = a.Run(ctx, SessionAgentCall{
			SessionID:    sessionID,
			Prompt:       continuationPromptPlaceholder,
			Continuation: true,
		})
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		// A resume that runs straight into the limit again schedules
		// itself anew through the ordinary path (completeTurn), so
		// there is nothing to retry here; anything else is reported by
		// the turn itself.
		slog.Error("Usage-limit resume failed", "session_id", sessionID, "error", err)
	}
}

// WaitingOnUsageLimit implements SessionAgent.
func (a *sessionAgent) WaitingOnUsageLimit(sessionID string) bool {
	return a.limitResumes.pending(sessionID)
}
