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
// internal/app/threadspawn.NewCoordinatorAdapter), exactly the way the
// Spawner/Handle and SessionService/MessageService seams already work.
//
// The types below are the domain's own spellings of the corresponding
// agent/notify types; the composition seam maps between them one-to-one,
// so lifecycle behavior is unchanged.

// runIDContextKey carries a per-run RunID from the dispatch site down into
// Coordinator.Run/RunAccepted. It mirrors agent.WithRunID but is owned here
// so thread does not import agent; the composition seam copies it onto the
// agent's own run-id key (see threadspawn's Coordinator adapter).
type runIDContextKey struct{}

// WithRunID returns ctx tagged with a per-run RunID. The adapter at the
// composition seam re-applies it to the agent's own run-id context key, so
// the coordinator's terminal RunComplete echoes the same RunID back.
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
// persisted under. It mirrors agent.WithPromptOrigin/OriginAgent but is
// owned here; the composition seam copies it onto the agent's own origin
// key.
type promptOriginContextKey struct{}

// WithAgentDispatch returns ctx tagged so the prompt dispatched through it
// is persisted as an agent-origin (not the person's own words) message. It
// is the thread-domain spelling of agent.WithAgentDispatch: the tag still
// lands in the coordinator's persistence path through the composition-seam
// adapter, so persistence behavior is identical.
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

// steeringContextKey carries a steering dispatch's decision hook. It
// mirrors agent.WithSteering but is owned here; the composition seam
// copies it onto the agent's own steering key.
type steeringContextKey struct{}

// DispatchOutcome is what a coordinator did with a steering dispatch, as
// reported through [WithSteering]'s hook. It is the domain's spelling of
// agent.SteerOutcome; the composition seam maps the three one-to-one.
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
	// folded into its next step, extending that turn instead of
	// sequencing a new one. It has no RunID of its own and produces no
	// terminal event: the turn it joined still reports for both.
	DispatchFolded
	// DispatchCancelled means a cancel covering this dispatch was already
	// recorded when it reached the coordinator, so it neither ran nor
	// folded. Reachable only because the dispatch reserves acceptance
	// (see Coordinator.BeginAccepted) — which is the point of reserving:
	// a cancel racing a dispatch resolves to a definite answer instead of
	// an unaccounted-for run.
	DispatchCancelled
)

// WithSteering returns ctx tagged so the prompt dispatched through it
// folds into the target session's turn in flight, if there is one,
// instead of queueing behind it. It is the thread-domain spelling of
// agent.WithSteering.
//
// onDispatch is called once with the decision the coordinator reached,
// from under its own dispatch mutex and before any turn starts streaming
// — the moment [lifecycle.steer] needs it, since it decides from that
// whether the delegation's workspace has a new owner. The hook must not
// block.
func WithSteering(ctx context.Context, onDispatch func(DispatchOutcome)) context.Context {
	return context.WithValue(ctx, steeringContextKey{}, onDispatch)
}

// SteeringFromContext returns the hook set by [WithSteering] and whether
// ctx was tagged at all. Exported so the composition seam can re-apply the
// tag to the agent's own steering context key.
func SteeringFromContext(ctx context.Context) (func(DispatchOutcome), bool) {
	v, ok := ctx.Value(steeringContextKey{}).(func(DispatchOutcome))
	return v, ok
}

// RunComplete is the domain's view of an agent run's terminal event — the
// thread-domain spelling of agent/notify.RunComplete. The composition seam
// maps a workspace's real RunComplete broker onto [RunCompletionBroker]
// (see threadspawn) and converts each event field-for-field, so the
// lifecycle observes exactly the same SessionID/RunID/Text/Error/Cancelled
// it used to read from notify.RunComplete.
type RunComplete struct {
	SessionID string
	RunID     string
	MessageID string
	Text      string
	Error     string
	Cancelled bool
}

// RunCompletionBroker is the slice of a workspace's run-completion event
// stream the lifecycle's per-run watcher uses: subscribe to it (to pick up
// a dispatched run's terminal event) and, for tests, publish a synthetic
// completion. It is satisfied by *pubsub.Broker[RunComplete] directly, and
// by the composition seam's adapter over a workspace's real
// *pubsub.Broker[notify.RunComplete].
type RunCompletionBroker interface {
	Subscribe(ctx context.Context) <-chan pubsub.Event[RunComplete]
	Publish(typ pubsub.EventType, v RunComplete)
}

// DelegationParent is the domain's view of the parent link a delegation
// registers so a mid-run ask resolves to the right coordinator and session.
// It is the thread-domain spelling of agent.DelegationParent; the
// composition seam maps the Parent [Coordinator] back to the concrete
// agent.Coordinator it wraps.
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

// TaskCompletion is the domain's view of a delegation's terminal-completion
// event delivered into its parent session's inbox. It is the
// thread-domain spelling of agent.TaskCompletion; the composition seam maps
// it one-to-one.
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
	// Acknowledge clears the durable outbox only after the parent turn has
	// successfully folded this completion into a model step.
	Acknowledge func(context.Context) error
}

// Attachment is the domain's view of a file the person attached to a
// prompt — the thread-domain spelling of message.Attachment, mapped
// field-for-field at the composition seam like every other type in this
// file.
//
// It exists because one dispatch path does carry attachments: the person
// typing into a thread's own session (see Manager.RunFromPerson). Every
// other path this package drives — a goal, a thread_send follow-up — is
// text an agent wrote, and passes none.
type Attachment struct {
	FilePath string
	FileName string
	MimeType string
	Content  []byte
}

// Coordinator is the slice of a workspace's agent coordinator the
// delegation lifecycle drives: dispatching a run (Run/RunAccepted, with the
// accept handle reserved by BeginAccepted), cancelling a session,
// registering a delegation's parent link, and delivering a terminal
// completion into a parent session. Declared here, on the consumer side, so
// internal/thread stays free of internal/agent.
//
// There is deliberately no unreserved dispatch here. Every prompt this
// package sends reaches the coordinator on a goroutine, so every one of
// them can race a cancel between being scheduled and being admitted;
// reserving acceptance first is what makes that race resolve to a
// definite answer rather than a run nobody accounted for. A port that
// also offered a bare Run would be offering that hole back.
type Coordinator interface {
	// RunAccepted dispatches prompt into sessionID as a fire-and-forget
	// run, reserved by an earlier [BeginAccepted], whose completion is
	// delivered through the workspace's RunCompletionBroker. err is
	// non-nil only for a pre-execution failure (the caller then
	// synthesizes the terminal event itself). accept is the
	// [BeginAccepted] result, carried opaquely so the port never names the
	// agent's AcceptedRun type. attachments is nil for every dispatch this
	// package makes on an agent's behalf; see [Attachment].
	RunAccepted(ctx context.Context, accept any, sessionID, prompt string, attachments []Attachment) error
	// BeginAccepted reserves acceptance for sessionID before a run is
	// dispatched, so cancellation cannot leave a run unaccounted for
	// between scheduling and coordinator admission.
	BeginAccepted(sessionID string) any
	// Cancel stops the run in flight in sessionID (never the whole
	// coordinator).
	Cancel(sessionID string)
	// SessionQueue reports whether sessionID is mid-turn right now and how
	// many prompts are already waiting behind that turn. It is a snapshot
	// read taken before a dispatch, purely so the caller can tell the
	// sender whether the message it just handed over will be read now or
	// only after the current turn ends — see [SendDisposition]. Nothing in
	// the lifecycle branches on it.
	SessionQueue(sessionID string) (busy bool, queued int)
	// RegisterDelegationParent records that the child session identified by
	// sessionID reports its completion / resolves mid-run asks against
	// parent.
	RegisterDelegationParent(sessionID string, parent DelegationParent)
	// DeliverTaskCompletion enqueues completion into the parent session's
	// inbox for delivery on its next step.
	DeliverTaskCompletion(ctx context.Context, parentSessionID string, completion TaskCompletion)
}
