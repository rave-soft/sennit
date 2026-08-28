package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// The idle summarize pass.
//
// Auto-summarize, until now, only ever fired from inside a turn: a step
// noticed the next request would not fit the context window and stopped to
// compress first (see stopOnContextWindow and finishTurn's shouldSummarize
// path). That is the right backstop and the wrong moment. The person is
// sitting there waiting for an answer, and the session spends its most
// expensive request of the day — a full replay of the whole conversation —
// before it can even start.
//
// This is the same compression, moved to the moment nobody is waiting: a
// session whose context has grown past a configured size and has then seen
// no work for a configured stretch is summarized where it stands. Its next
// turn starts on a compacted context, and the cost was paid into silence.
//
// It is deliberately crude, for the same reason internal/thread's idle
// watchdog is: it asks "has this session done anything lately, and is it
// carrying enough context to be worth compressing", and nothing else. What
// keeps that safe is that summarize is idempotent in the way that matters —
// it zeroes the session's prompt tokens (see sessionAgent.summarize), so a
// session it has just compressed falls below the threshold and is not
// picked up again until it has genuinely grown back.
const (
	// idleSummarizeSweepInterval is how often the sweep runs. It bounds
	// how late a trip can be (the configured idle window plus one
	// interval) and is deliberately coarse: each pass is one map walk
	// plus one indexed session read per candidate, and a summarize that
	// happens half a minute later than it could have costs nothing —
	// nobody is waiting on it.
	idleSummarizeSweepInterval = 30 * time.Second
)

// markActivity records that sessionID is doing something right now. Called
// at both ends of a turn rather than only at the end: a turn that runs for
// longer than the idle window must not look idle while it is still running
// (IsSessionBusy already covers that, but only for as long as the dispatch
// state exists), and a turn that ends leaves the clock running from its own
// end, which is when the person actually went quiet.
//
// Only the top-level dispatcher's own entry points call this, so the sweep
// never sees a delegation's private session: those are work nobody is
// sitting in, they end on their own, and compressing one mid-flight would
// rewrite a delegation's context out from under it.
func (d *turnDispatcher) markActivity(sessionID string) {
	// A dispatcher built without the map (the bare struct literals a few
	// tests use) simply has no idle pass; nothing else in a turn depends
	// on this, so it must never be the thing that panics one.
	if sessionID == "" || d.lastActivity == nil {
		return
	}
	d.lastActivity.Set(sessionID, time.Now())
}

// startIdleSummarize runs the idle sweep for as long as the coordinator's
// readiness lifecycle lives. Registering through lifecycle.launch is what
// makes Close join it: close cancels the shared context and then waits on
// the same group this goroutine is counted in.
//
// A lifecycle already closing gets no watchdog, which is correct — there is
// nothing left to summarize into.
func (d *turnDispatcher) startIdleSummarize() {
	d.lifecycle.launch(&d.idleSummarizeGroup, func(ctx context.Context) error {
		ticker := time.NewTicker(idleSummarizeSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				d.sweepIdleSessions(ctx, time.Now())
			}
		}
	})
}

// sweepIdleSessions summarizes every session that has been idle past the
// configured window while carrying more than the configured context. now is
// passed in rather than read here so tests can drive the clock.
//
// The config is read once per sweep, not captured at construction: turning
// the pass off, or moving its thresholds, takes effect on the next tick
// without a restart.
func (d *turnDispatcher) sweepIdleSessions(ctx context.Context, now time.Time) {
	if d.lastActivity == nil {
		return
	}
	cfg := d.cfg.Config().Options
	if cfg == nil || cfg.DisableAutoSummarize || !cfg.AutoSummarizeIdle.IsEnabled() {
		return
	}
	var (
		after     = cfg.AutoSummarizeIdle.EffectiveAfter()
		threshold = cfg.AutoSummarizeIdle.EffectiveContextTokens()
	)
	for sessionID, last := range d.lastActivity.Seq2() {
		if ctx.Err() != nil {
			return
		}
		if now.Sub(last) < after {
			continue
		}
		d.summarizeIfIdle(ctx, sessionID, threshold)
	}
}

// summarizeIfIdle runs the pass for one candidate session: it re-checks
// everything that can have changed since the sweep picked it, then
// summarizes.
//
// A session that turns out to be busy is skipped rather than attempted:
// summarize would refuse it with ErrSessionBusy anyway, and building a
// runtime to find that out is the expensive half of the call.
func (d *turnDispatcher) summarizeIfIdle(ctx context.Context, sessionID string, threshold int64) {
	agent := d.agentPort.current()
	if agent == nil || agent.IsSessionBusy(sessionID) {
		return
	}
	sess, err := d.sessions.Get(ctx, sessionID)
	if err != nil {
		// A session that no longer exists (deleted while this process
		// held it) is not an error worth repeating every 30 seconds:
		// drop it and stop watching. Anything else is logged and
		// retried on the next sweep — an unreadable store must never
		// be the reason a session is summarized, or forgotten.
		if errors.Is(err, context.Canceled) {
			return
		}
		slog.Debug("Could not read a session for the idle summarize sweep",
			"session_id", sessionID, "error", err)
		d.lastActivity.Del(sessionID)
		return
	}
	if sess.PromptTokens < threshold {
		return
	}

	// Mark first, so a summarize that fails (or one that takes a while)
	// does not leave the session looking idle enough to be retried on the
	// very next tick. A real failure is worth one attempt per idle
	// window, not one every 30 seconds.
	d.markActivity(sessionID)
	slog.Info("Summarizing an idle session",
		"session_id", sessionID, "prompt_tokens", sess.PromptTokens)
	summarize := d.summarizeIdle
	if summarize == nil {
		summarize = d.Summarize
	}
	if err := summarize(ctx, sessionID); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionBusy) {
			return
		}
		slog.Warn("Idle summarize failed", "session_id", sessionID, "error", err)
		return
	}
	// The summarize wrote a summary message and zeroed the session's
	// prompt tokens, so the clock restarts from a session that is now
	// below the threshold either way — but keep the mark honest.
	d.markActivity(sessionID)
}
