package thread

// Status is the lifecycle state of a thread.
type Status string

const (
	StatusPending Status = "pending"
	StatusRunning Status = "running"
	// StatusIdle is a thread whose worktree, branch, and workspace are
	// live but which has no agent run in flight: created without a goal
	// to isolate work the user drives themselves, or reactivated by
	// [Manager.Activate] after an earlier run finished. It is neither
	// active (nothing is running) nor terminal (the thread is not
	// finished), so destructive consumers such as braid gc leave it
	// alone.
	StatusIdle         Status = "idle"
	StatusCompleted    Status = "completed"
	StatusMerging      Status = "merging"
	StatusMerged       Status = "merged"
	StatusConflict     Status = "conflict"
	StatusMergeBlocked Status = "merge_blocked"
	StatusFailed       Status = "failed"
	StatusInterrupted  Status = "interrupted"
)

// Active reports whether the thread still has work in flight: pending,
// running, or merging. Idle threads are deliberately excluded: their
// workspace is live, but nothing is executing in it.
func (s Status) Active() bool {
	switch s {
	case StatusPending, StatusRunning, StatusMerging:
		return true
	default:
		return false
	}
}

// Terminal reports whether the thread is known to be finished. This is
// deliberately not !Active(): a status this build doesn't know (from a
// newer version sharing the database) is neither active nor terminal, so
// destructive consumers (braid gc) leave it alone. StatusIdle is
// likewise neither, for the same reason: an idle thread is work in
// progress that simply has no run of its own in flight.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusMerged, StatusConflict,
		StatusMergeBlocked, StatusFailed, StatusInterrupted:
		return true
	default:
		return false
	}
}

// MergePolicy controls how a completed thread's branch is merged back into
// its base branch.
type MergePolicy string

const (
	MergeAuto   MergePolicy = "auto"
	MergeManual MergePolicy = "manual"
)

// Thread is a parallel agent work stream running in its own git worktree.
type Thread struct {
	ID            string
	Name          string
	Goal          string
	BaseBranch    string
	Branch        string
	WorktreePath  string
	SessionID     string
	Status        Status
	MergePolicy   MergePolicy
	ResultSummary string
	Error         string
	CreatedAt     int64
	UpdatedAt     int64
	CompletedAt   int64
}
