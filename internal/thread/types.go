package thread

// Status is the lifecycle state of a thread.
type Status string

const (
	StatusPending      Status = "pending"
	StatusRunning      Status = "running"
	StatusCompleted    Status = "completed"
	StatusMerging      Status = "merging"
	StatusMerged       Status = "merged"
	StatusConflict     Status = "conflict"
	StatusMergeBlocked Status = "merge_blocked"
	StatusFailed       Status = "failed"
	StatusInterrupted  Status = "interrupted"
)

// Active reports whether the thread still has work in flight: pending,
// running, or merging.
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
// destructive consumers (braid gc) leave it alone.
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
