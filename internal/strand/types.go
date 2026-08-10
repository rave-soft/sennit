package strand

// Status is the lifecycle state of a strand.
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

// MergePolicy controls how a completed strand's branch is merged back into
// its base branch.
type MergePolicy string

const (
	MergeAuto   MergePolicy = "auto"
	MergeManual MergePolicy = "manual"
)

// Strand is a parallel agent work stream running in its own git worktree.
type Strand struct {
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
