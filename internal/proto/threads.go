package proto

type ThreadStatus string

const (
	ThreadStatusPending      ThreadStatus = "pending"
	ThreadStatusRunning      ThreadStatus = "running"
	ThreadStatusIdle         ThreadStatus = "idle"
	ThreadStatusCompleted    ThreadStatus = "completed"
	ThreadStatusFailed       ThreadStatus = "failed"
	ThreadStatusInterrupted  ThreadStatus = "interrupted"
	ThreadStatusCancelled    ThreadStatus = "cancelled"
	ThreadStatusMerging      ThreadStatus = "merging"
	ThreadStatusMerged       ThreadStatus = "merged"
	ThreadStatusConflict     ThreadStatus = "conflict"
	ThreadStatusMergeBlocked ThreadStatus = "merge_blocked"
)

func (s ThreadStatus) Active() bool {
	return s == ThreadStatusPending || s == ThreadStatusRunning || s == ThreadStatusMerging
}

func (s ThreadStatus) Terminal() bool {
	switch s {
	case ThreadStatusCompleted, ThreadStatusMerged, ThreadStatusConflict, ThreadStatusMergeBlocked, ThreadStatusFailed, ThreadStatusInterrupted, ThreadStatusCancelled:
		return true
	default:
		return false
	}
}

type ThreadKind string

const (
	ThreadKindThread ThreadKind = "thread"
	ThreadKindTask   ThreadKind = "task"
)

// Thread is the wire representation of a thread (see internal/thread.Thread).
type Thread struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Goal         string `json:"goal"`
	BaseBranch   string `json:"base_branch"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	// WorkspaceID is the runtime workspace ID of the thread's currently
	// spawned isolated workspace (see internal/thread.Manager.WorkspaceID),
	// empty when the thread's workspace is not currently spawned (e.g.
	// completed/merged/failed, or interrupted and not yet resumed).
	WorkspaceID string `json:"workspace_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Status      string `json:"status"`
	// Kind discriminates the delegation kinds sharing the threads table
	// (see internal/thread.Kind); every value the server sends today is
	// "thread". Additive field: older clients that don't read it are
	// unaffected.
	Kind          string `json:"kind"`
	MergePolicy   string `json:"merge_policy"`
	ResultSummary string `json:"result_summary,omitempty"`
	Error         string `json:"error,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	CompletedAt   int64  `json:"completed_at,omitempty"`
	// ParentSessionID is the session this delegation reports its
	// completion (and any mid-run ask) to, if any — see
	// internal/thread.Delegation.ParentSessionID. Additive field: older
	// clients that don't read it are unaffected.
	ParentSessionID string `json:"parent_session_id,omitempty"`
}

// CreateThreadRequest is the request body for creating a thread.
type CreateThreadRequest struct {
	Name        string `json:"name"`
	Goal        string `json:"goal"`
	BaseBranch  string `json:"base_branch,omitempty"`
	MergePolicy string `json:"merge_policy,omitempty"`
	// ParentSessionID is the session the thread was started from. It is
	// what lets the thread's completion reach the agent that is waiting
	// for it: without it the delegation has nobody to report to and its
	// result is only discoverable by looking. Empty when a thread is
	// created outside any session, as the CLI does.
	ParentSessionID string `json:"parent_session_id,omitempty"`
}

// RemoveThreadOptions controls thread removal.
type RemoveThreadOptions struct {
	Force        bool `json:"force,omitempty"`
	DeleteBranch bool `json:"delete_branch,omitempty"`
}

// ThreadEventType identifies the kind of lifecycle change carried by a
// ThreadEvent, mirroring internal/thread.EventType.
type ThreadEventType string

const (
	ThreadEventCreated       ThreadEventType = "created"
	ThreadEventStatusChanged ThreadEventType = "status_changed"
	ThreadEventMerged        ThreadEventType = "merged"
	ThreadEventRemoved       ThreadEventType = "removed"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (t ThreadEventType) MarshalText() ([]byte, error) {
	return []byte(t), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (t *ThreadEventType) UnmarshalText(text []byte) error {
	*t = ThreadEventType(text)
	return nil
}

// ThreadEvent is published over SSE on every thread lifecycle change.
type ThreadEvent struct {
	Type   ThreadEventType `json:"type"`
	Thread Thread          `json:"thread"`
}
