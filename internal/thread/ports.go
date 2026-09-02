package thread

import (
	"context"
	"time"

	"github.com/rave-soft/sennit/internal/pubsub"
)

// This file declares the narrow, dependency-free ports the delegation
// lifecycle needs from the workspace's agent coordinator, so that
// internal/thread never imports internal/agent (and, transitively,
// internal/config and internal/db). The concrete agent.Coordinator is
// adapted to [Coordinator] at the composition seam (see
// internal/app/threadspawn.NewCoordinatorAdapter). The types below are the
// domain's own spellings of the corresponding agent/notify types, mapped
// one-to-one at that seam.

// runIDContextKey carries a per-run RunID from the dispatch site down into
// Coordinator.Run/RunAccepted; the composition seam copies it onto the
// agent's own run-id key.
type runIDContextKey struct{}

// WithRunID returns ctx tagged with a per-run RunID, so the coordinator's
// terminal RunComplete echoes it back.
func WithRunID(ctx context.Context, runID string) context.Context {
	return context.WithValue(ctx, runIDContextKey{}, runID)
}

// RunIDFromContext returns the RunID set by [WithRunID], or "" if none.
// Exported so the composition seam can re-tag the agent's context with it.
func RunIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(runIDContextKey{}).(string); ok {
		return v
	}
	return ""
}

// promptOriginContextKey carries the origin a dispatched prompt should be
// persisted under.
type promptOriginContextKey struct{}

// WithAgentDispatch returns ctx tagged so the prompt dispatched through it
// is persisted as an agent-origin message rather than the person's own
// words.
func WithAgentDispatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, promptOriginContextKey{}, true)
}

// AgentDispatchFromContext reports whether [WithAgentDispatch] tagged ctx.
// Exported so the composition seam can re-apply the tag to the agent's own
// origin context key.
func AgentDispatchFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(promptOriginContextKey{}).(bool)
	return v
}

// steeringContextKey carries a steering dispatch's decision hook.
type steeringContextKey struct{}

// DispatchOutcome is what a coordinator did with a steering dispatch, as
// reported through [WithSteering]'s hook.
//
// Only [DispatchRan] produces a run the lifecycle can own — that is the
// whole reason this is reported at all, rather than inferred afterwards
// from a queue probe that would race the turn it is about.
type DispatchOutcome int

const (
	// DispatchRan means the session was idle and the prompt became its
	// own run, under the RunID the dispatching context carried.
	DispatchRan DispatchOutcome = iota
	// DispatchFolded means a turn was already in flight and the prompt
	// folded into its next step. It has no RunID of its own and produces
	// no terminal event: the turn it joined still reports for both.
	DispatchFolded
	// DispatchCancelled means a cancel covering this dispatch was already
	// recorded when it reached the coordinator. Reachable only because
	// the dispatch reserves acceptance first (see
	// Coordinator.BeginAccepted), so a cancel racing a dispatch resolves
	// to a definite answer instead of an unaccounted-for run.
	DispatchCancelled
)

// WithSteering returns ctx tagged so the prompt dispatched through it
// folds into the target session's turn in flight, if there is one,
// instead of queueing behind it.
//
// onDispatch is called once with the decision the coordinator reached,
// from under its own dispatch mutex and before any turn starts streaming
// — the moment [lifecycle.steer] needs it to decide whether the
// delegation's workspace has a new owner. The hook must not block.
func WithSteering(ctx context.Context, onDispatch func(DispatchOutcome)) context.Context {
	return context.WithValue(ctx, steeringContextKey{}, onDispatch)
}

// SteeringFromContext returns the hook set by [WithSteering] and whether
// ctx was tagged at all.
func SteeringFromContext(ctx context.Context) (func(DispatchOutcome), bool) {
	v, ok := ctx.Value(steeringContextKey{}).(func(DispatchOutcome))
	return v, ok
}

// RunComplete is the domain's view of an agent run's terminal event. The
// composition seam maps a workspace's real RunComplete broker onto
// [RunCompletionBroker] and converts each event field-for-field.
type RunComplete struct {
	SessionID string
	RunID     string
	MessageID string
	Text      string
	Error     string
	Cancelled bool
}

// RunCompletionBroker is the slice of a workspace's run-completion event
// stream the lifecycle's per-run watcher subscribes to for a dispatched
// run's terminal event (and, for tests, publishes a synthetic completion
// through).
type RunCompletionBroker interface {
	Subscribe(ctx context.Context) <-chan pubsub.Event[RunComplete]
	Publish(typ pubsub.EventType, v RunComplete)
}

// DelegationParent is the parent link a delegation registers so a mid-run
// ask resolves to the right coordinator and session.
type DelegationParent struct {
	// Parent is the coordinator owning the parent session's completion
	// inbox (for a thread, a different coordinator than its own; for a
	// task, its own).
	Parent          Coordinator
	ParentSessionID string
	DelegationID    string
	Kind            string
	Name            string
	Depth           int
}

// TaskCompletion is a delegation's terminal-completion event delivered into
// its parent session's inbox.
type TaskCompletion struct {
	DelegationID   string
	Kind           string
	Name           string
	Goal           string
	Status         string
	ChildSessionID string
	ResultText     string
	Error          string
	Depth          int
	TerminalAt     time.Time
	// PriorReports is how many terminal completions this same delegation
	// has already delivered to this same parent - 0 for the ordinary
	// case. See threadControl.reports for why it is ever anything else.
	PriorReports int
	// Acknowledge clears the durable outbox only after the parent turn has
	// successfully folded this completion into a model step.
	Acknowledge func(context.Context) error
}

// Attachment is the domain's view of a file the person attached to a
// prompt — the thread-domain spelling of message.Attachment, mapped
// field-for-field at the composition seam. Only one dispatch path carries
// attachments: the person typing into a thread's own session (see
// Manager.RunFromPerson). Every other path this package drives passes none.
type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
}

// AcceptedRun is an opaque reservation returned by [Coordinator.BeginAccepted].
// A coordinator consumes it when it admits the dispatch; callers must close it
// when RunAccepted returns an error before admission. Its handle is private so
// the lifecycle cannot depend on a coordinator's concrete reservation type.
type AcceptedRun struct {
	handle interface{ Close() }
}

// NewAcceptedRun carries handle through the consumer-owned coordinator port.
// It is used only by composition adapters that own a concrete coordinator.
func NewAcceptedRun(handle interface{ Close() }) *AcceptedRun {
	return &AcceptedRun{handle: handle}
}

// Close releases a reservation which was not consumed by a coordinator.
func (r *AcceptedRun) Close() {
	if r != nil && r.handle != nil {
		r.handle.Close()
	}
}

// Handle returns the opaque reservation for the composition adapter that
// created it. The lifecycle must use [AcceptedRun.Close] instead.
func (r *AcceptedRun) Handle() interface{ Close() } {
	if r == nil {
		return nil
	}
	return r.handle
}

// Coordinator is the slice of a workspace's agent coordinator the
// delegation lifecycle drives. Declared here, on the consumer side, so
// internal/thread stays free of internal/agent.
//
// There is deliberately no unreserved dispatch here: every prompt this
// package sends can race a cancel between being scheduled and being
// admitted, and reserving acceptance first (BeginAccepted) is what makes
// that race resolve to a definite answer rather than a run nobody
// accounted for.
type Coordinator interface {
	// RunAccepted dispatches prompt into sessionID as a fire-and-forget
	// run, reserved by an earlier [BeginAccepted], whose completion is
	// delivered through the workspace's RunCompletionBroker. err is
	// non-nil only for a pre-execution failure (the caller then
	// synthesizes the terminal event itself). attachments is nil for
	// every dispatch this package makes on an agent's behalf; see
	// [Attachment].
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments []Attachment) error
	// BeginAccepted reserves acceptance for sessionID before a run is
	// dispatched, so cancellation cannot leave a run unaccounted for
	// between scheduling and coordinator admission.
	BeginAccepted(sessionID string) *AcceptedRun
	// Cancel stops the run in flight in sessionID (never the whole
	// coordinator).
	Cancel(sessionID string)
	// SessionQueue reports whether sessionID is mid-turn right now and how
	// many prompts are already queued behind it — see [SendDisposition].
	SessionQueue(sessionID string) (busy bool, queued int)
	// RegisterDelegationParent records that the child session identified by
	// sessionID reports its completion / resolves mid-run asks against
	// parent.
	RegisterDelegationParent(sessionID string, parent DelegationParent)
	// SetLiveSession tells this coordinator which one session it is
	// working in. See agent.Coordinator.SetLiveSession.
	SetLiveSession(sessionID string)
	// DeliverTaskCompletion enqueues completion into the parent session's
	// inbox for delivery on its next step.
	DeliverTaskCompletion(ctx context.Context, parentSessionID string, completion TaskCompletion)
}
