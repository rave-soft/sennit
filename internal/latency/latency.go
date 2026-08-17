// Package latency records how long two internal handoffs waited before
// they actually reached the model.
//
// Both handoffs are asynchronous by design and both can silently get
// slower without anything failing: a steering message sits in the queue
// until a step drains it, and a finished background delegation sits in
// the completion inbox until the parent's next step folds it in. Until
// this package existed the two waits were only ever emitted as
// structured logs (`waited_ms` on "Steering folded into turn" and
// "Completion delivered" in internal/agent/turn.go), so a regression was
// visible only to someone reading logs by hand. Recording them makes the
// distribution queryable after the fact, via `sennit stat --by latency`.
//
// # What a recorded wait means
//
// The clock starts at the instant the thing became deliverable, not at
// the instant it was created: for steering that is when the follow-up
// was accepted into the queue, for a delegation it is when the
// delegation reached a terminal status. It stops at the instant the
// content was appended to a step that then went on to succeed. A step
// that fails before reaching the model records nothing — the content
// goes back on the queue and is timed again on the attempt that
// actually delivers it, so a wait is always the full wait.
//
// A recorded wait is therefore dominated by how long the parent was
// busy, not by any queue processing cost. Long tails are expected when a
// parent is mid-stream; what a regression looks like is the *floor*
// rising, or the tail growing without a matching change in turn length.
package latency

import (
	"context"
	"log/slog"
	"time"

	"github.com/rave-soft/sennit/internal/db"
)

// Kind names one of the two handoffs. The values are stored in the
// database as-is, so they are part of the on-disk format: renaming one
// orphans every row already recorded under the old name.
type Kind string

const (
	// KindSteeringFold is the wait from a steering follow-up being
	// queued to it being folded into a step.
	KindSteeringFold Kind = "steering_fold"
	// KindCompletionDelivery is the wait from a background delegation
	// reaching a terminal status to its completion being delivered to
	// the parent session.
	KindCompletionDelivery Kind = "completion_delivery"
)

// Recorder records observed waits. It is deliberately fire-and-forget:
// this is measurement, and a turn must never fail, block, or change
// behavior because measuring it did not work.
type Recorder interface {
	// Record stores one observed wait. A negative duration is dropped
	// rather than clamped — it means the two clocks it was derived from
	// disagree, and a zero would read as "instant" rather than "bogus".
	Record(ctx context.Context, kind Kind, sessionID string, waited time.Duration)
}

type service struct {
	q *db.Queries
}

// NewService returns a Recorder that writes to the shared database.
func NewService(q *db.Queries) Recorder {
	return &service{q: q}
}

func (s *service) Record(ctx context.Context, kind Kind, sessionID string, waited time.Duration) {
	if sessionID == "" || waited < 0 {
		return
	}
	// WithoutCancel: the callers record at the end of a step, and the
	// context they hold is the turn's. A cancel arriving in that window
	// would otherwise drop the measurement of the very turn most worth
	// measuring — the slow one. The write is a single-row insert, so
	// outliving the cancel costs nothing.
	if err := s.q.RecordLatencyEvent(context.WithoutCancel(ctx), db.RecordLatencyEventParams{
		SessionID: sessionID,
		Kind:      string(kind),
		WaitedMs:  waited.Milliseconds(),
	}); err != nil {
		// Debug, not Error: the most likely cause is a session row that
		// no longer exists (the foreign key), which is normal for a
		// session deleted mid-turn and not worth shouting about in a
		// path that exists purely to observe.
		slog.Debug("Recording latency event failed", "error", err, "kind", kind, "session", sessionID)
	}
}
