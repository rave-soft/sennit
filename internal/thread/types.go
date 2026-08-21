package thread

// Status is the lifecycle state shared by every delegation kind this
// package manages. [Status.Active] and [Status.Terminal] are exhaustive
// over every value below — core and overlay alike — even though only
// [Thread] ever reaches the overlay-only values below, so that a status
// this build doesn't recognize (from a newer overlay sharing the
// database) safely reports neither.
type Status string

// Core statuses: reachable by any delegation, with no dependency on
// Thread's git-worktree/merge overlay.
const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	// StatusIdle is a delegation whose workspace is live but has no agent
	// run in flight: created without a goal to isolate work the user
	// drives themselves, or reactivated by [Manager.Activate] after an
	// earlier run finished. It is neither active (nothing is running) nor
	// terminal (the delegation is not finished), so destructive consumers
	// such as sennit gc leave it alone.
	StatusIdle        Status = "idle"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
	// StatusCancelled is a terminal status reached only through an
	// explicit Cancel call (TaskManager.Cancel or Manager.Cancel) — the
	// user (or the model, via task_cancel) deliberately stopped this
	// delegation, as opposed to StatusInterrupted, which covers every
	// incidental way a run stops without anyone choosing to (a process
	// restart reconciled by lifecycle.recover, Manager.Shutdown releasing
	// live runtimes, or the run itself reporting RunComplete.Cancelled).
	// Core, not overlay: both a task and a thread can be cancelled this
	// way, unlike the git-worktree/merge statuses below which only a
	// thread ever reaches.
	StatusCancelled Status = "cancelled"
)

// Overlay statuses: reachable only by a [Thread], as part of its
// git-worktree merge flow.
const (
	StatusMerging      Status = "merging"
	StatusMerged       Status = "merged"
	StatusConflict     Status = "conflict"
	StatusMergeBlocked Status = "merge_blocked"
)

// Active reports whether the delegation still has work in flight: pending,
// running, or (Thread overlay) merging. Idle delegations are deliberately
// excluded: their workspace is live, but nothing is executing in it.
func (s Status) Active() bool {
	switch s {
	case StatusPending, StatusRunning, StatusMerging:
		return true
	default:
		return false
	}
}

// Terminal reports whether the delegation is known to be finished. This is
// deliberately not !Active(): a status this build doesn't know (from a
// newer version sharing the database) is neither active nor terminal, so
// destructive consumers (sennit gc) leave it alone. StatusIdle is likewise
// neither, for the same reason: an idle delegation is work in progress
// that simply has no run of its own in flight.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusMerged, StatusConflict,
		StatusMergeBlocked, StatusFailed, StatusInterrupted, StatusCancelled:
		return true
	default:
		return false
	}
}

// Kind discriminates the delegation kinds sharing the threads table (see
// [Delegation.Kind]).
type Kind string

const (
	// KindThread is the value every [Thread] writes: a delegation that
	// lives in its own git worktree and branch, with a merge policy.
	KindThread Kind = "thread"
	// KindTask is the lightweight, worktree-less delegation kind built on
	// top of this same table; see [TaskManager.Create].
	KindTask Kind = "task"
)

// Delegation is the core record every background delegation this package
// manages carries, regardless of overlay: identity, goal, the session
// driving it, lifecycle status/outcome, and which overlay (Kind) owns it.
// [Thread] embeds it and adds the git-worktree-specific fields its overlay
// needs.
type Delegation struct {
	ID            string
	Name          string
	Goal          string
	SessionID     string
	Status        Status
	Kind          Kind
	ResultSummary string
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   int64
	// ParentSessionID is the session this delegation's own session nests
	// under, persisted so the link survives a process restart — the
	// durable counterpart of the old in-memory-only
	// threadControl.parentSessionID, which now only feeds
	// TaskManager.checkActiveCaps's admission cache. Empty means no
	// parent, exactly as before: resolveDeliveryTarget and SendToParent
	// both treat "" as nobody to deliver/ask.
	ParentSessionID string
}

// MergePolicy controls how a completed thread's branch is merged back into
// its base branch.
type MergePolicy string

const (
	MergeAuto   MergePolicy = "auto"
	MergeManual MergePolicy = "manual"
)

// SendDisposition reports what actually happened to a message handed to
// [Manager.Send] or [TaskManager.Send]. A send always succeeds in the
// sense that the message is durably on its way, but "on its way" covers
// two very different outcomes for the sender: the delegation's agent
// either starts reading it right away, or it sits in the session's prompt
// queue until whatever turn is currently in flight finishes — which, for
// an agent deep in a long sub-agent call, can be many minutes.
//
// The distinction exists because a sender that cannot tell them apart
// makes bad decisions: a steering message ("you have five minutes left,
// wrap up") that lands in a queue behind a half-hour turn is not steering
// anything, yet a bare "sent" told the sender it was. The send tools
// render this back to the model verbatim — see tools.SendOutcome.
type SendDisposition struct {
	// Queued is true when the target session was mid-turn at dispatch, so
	// the message becomes the next prompt rather than the current one.
	Queued bool
	// Ahead is how many prompts were already waiting in the session's
	// queue when this one was added, i.e. how many turns run before it.
	// Only meaningful when Queued is true.
	Ahead int
	// Resumed is true when the delegation's workspace was not live and had
	// to be respawned to take the message. Such a send is never Queued:
	// a freshly resumed workspace has no turn in flight.
	Resumed bool
	// Steered is true when the message reached the turn already in flight
	// instead of waiting for it — the person's own sends only (see
	// [SenderPerson]), and only when there was a turn to reach. It is the
	// one outcome where "mid-turn" is not the same as "not read yet", so
	// it is reported apart from Queued rather than folded into it.
	Steered bool
}

// Sender says who wrote the message handed to [Manager.Send] /
// [Manager.SendFromPerson] / [TaskManager.Send]. It decides two things
// that only differ by authorship: the origin the message is persisted
// under, and whether it may fold into a turn already in flight.
type Sender int

const (
	// SenderAgent is a thread_send/task_send follow-up: an agent writing
	// to another agent. It is persisted as agent-origin, and it never
	// folds — a delegation's turn is not something another agent gets to
	// interrupt (see internal/config.toolnames on why thread_send is not
	// even in the default tool set).
	SenderAgent Sender = iota
	// SenderPerson is the person's own words, typed into the delegation's
	// session in the TUI. It is persisted as person-origin, and it folds
	// into the turn in flight when there is one, because "wait, do this
	// instead" that arrives after the work is done has steered nothing.
	SenderPerson
)

// Thread is a [Delegation] that additionally runs in its own git worktree
// and branch, and is by default folded back into a base branch on
// completion according to MergePolicy.
type Thread struct {
	Delegation
	BaseBranch   string
	Branch       string
	WorktreePath string
	MergePolicy  MergePolicy
}
