package proto

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
}

// CreateThreadRequest is the request body for creating a thread.
type CreateThreadRequest struct {
	Name        string `json:"name"`
	Goal        string `json:"goal"`
	BaseBranch  string `json:"base_branch,omitempty"`
	MergePolicy string `json:"merge_policy,omitempty"`
}

// SendThreadRequest is the request body for sending a follow-up message to
// a thread's session.
type SendThreadRequest struct {
	Message string `json:"message"`
}

// RemoveThreadOptions controls thread removal (query params on the DELETE
// endpoint — see internal/server).
type RemoveThreadOptions struct {
	Force        bool `json:"force,omitempty"`
	DeleteBranch bool `json:"delete_branch,omitempty"`
}

// CancelDelegationRequest is the request body for cancelling a delegation
// (task or thread) — both kinds' cancel routes take the same shape, since
// the underlying mechanics are shared (see internal/thread.lifecycle.cancel,
// under TaskManager.Cancel and Manager.Cancel).
type CancelDelegationRequest struct {
	// Reason is recorded as the delegation's terminal Error. Empty
	// defaults to "cancelled" server-side.
	Reason string `json:"reason,omitempty"`
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
