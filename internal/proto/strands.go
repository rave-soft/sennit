package proto

// Strand is the wire representation of a strand (see internal/strand.Strand).
type Strand struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Goal         string `json:"goal"`
	BaseBranch   string `json:"base_branch"`
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	// WorkspaceID is the runtime workspace ID of the strand's currently
	// spawned isolated workspace (see internal/strand.Manager.WorkspaceID),
	// empty when the strand's workspace is not currently spawned (e.g.
	// completed/merged/failed, or interrupted and not yet resumed).
	WorkspaceID   string `json:"workspace_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	Status        string `json:"status"`
	MergePolicy   string `json:"merge_policy"`
	ResultSummary string `json:"result_summary,omitempty"`
	Error         string `json:"error,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
	CompletedAt   int64  `json:"completed_at,omitempty"`
}

// CreateStrandRequest is the request body for creating a strand.
type CreateStrandRequest struct {
	Name        string `json:"name"`
	Goal        string `json:"goal"`
	BaseBranch  string `json:"base_branch,omitempty"`
	MergePolicy string `json:"merge_policy,omitempty"`
}

// SendStrandRequest is the request body for sending a follow-up message to
// a strand's session.
type SendStrandRequest struct {
	Message string `json:"message"`
}

// RemoveStrandOptions controls strand removal (query params on the DELETE
// endpoint — see internal/server).
type RemoveStrandOptions struct {
	Force        bool `json:"force,omitempty"`
	DeleteBranch bool `json:"delete_branch,omitempty"`
}

// StrandEventType identifies the kind of lifecycle change carried by a
// StrandEvent, mirroring internal/strand.EventType.
type StrandEventType string

const (
	StrandEventCreated       StrandEventType = "created"
	StrandEventStatusChanged StrandEventType = "status_changed"
	StrandEventMerged        StrandEventType = "merged"
	StrandEventRemoved       StrandEventType = "removed"
)

// MarshalText implements the [encoding.TextMarshaler] interface.
func (t StrandEventType) MarshalText() ([]byte, error) {
	return []byte(t), nil
}

// UnmarshalText implements the [encoding.TextUnmarshaler] interface.
func (t *StrandEventType) UnmarshalText(text []byte) error {
	*t = StrandEventType(text)
	return nil
}

// StrandEvent is published over SSE on every strand lifecycle change.
type StrandEvent struct {
	Type   StrandEventType `json:"type"`
	Strand Strand          `json:"strand"`
}
