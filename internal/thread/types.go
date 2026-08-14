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
	// such as braid gc leave it alone.
	StatusIdle        Status = "idle"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
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
// destructive consumers (braid gc) leave it alone. StatusIdle is likewise
// neither, for the same reason: an idle delegation is work in progress
// that simply has no run of its own in flight.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusMerged, StatusConflict,
		StatusMergeBlocked, StatusFailed, StatusInterrupted:
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
	// KindTask is reserved for the lightweight, worktree-less delegation
	// kind planned on top of this same table. Nothing constructs it yet.
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
}

// MergePolicy controls how a completed thread's branch is merged back into
// its base branch.
type MergePolicy string

const (
	MergeAuto   MergePolicy = "auto"
	MergeManual MergePolicy = "manual"
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
