package thread

import "context"

// CreateParams holds the fields needed to create a new thread. Status
// defaults to StatusPending, MergePolicy defaults to MergeAuto, and Kind
// defaults to KindThread when left unset.
type CreateParams struct {
	Name         string
	Goal         string
	BaseBranch   string
	Branch       string
	WorktreePath string
	SessionID    string
	MergePolicy  MergePolicy
	Kind         Kind
	// ParentSessionID is the durable counterpart of the old in-memory-only
	// threadControl.parentSessionID (see [Delegation.ParentSessionID]):
	// the session this delegation's own session nests under, persisted so
	// it survives a process restart. Empty means no parent.
	ParentSessionID string
}

// SetStatusParams holds the fields updated by a status transition.
type SetStatusParams struct {
	Status        Status
	Error         string
	ResultSummary string
	// CompletedAt is a Unix timestamp in seconds. Zero leaves the column
	// unset (NULL).
	CompletedAt int64
}

// Store persists threads. It is a thin persistence contract with no
// pub/sub of its own — the thread manager owns lifecycle events built on
// top of these operations. The sqlc-backed implementation lives in
// internal/app/threadspawn (NewStore): this domain package declares the
// contract and must not import internal/db.
type Store interface {
	Create(ctx context.Context, params CreateParams) (Thread, error)
	Get(ctx context.Context, id string) (Thread, error)
	GetByName(ctx context.Context, name string) (Thread, error)
	// List returns every kind = 'thread' row. Thread-facing callers
	// (thread_list, the dashboard, gc) want this. The generic lifecycle
	// recovery sweep must not: use ListAll instead, or a delegation of a
	// different kind left running when the process died would never be
	// reconciled.
	List(ctx context.Context) ([]Thread, error)
	// ListAll returns every delegation kind sharing this table (threads
	// today, tasks once they exist), scoped to project but not kind. This
	// is what the generic lifecycle recovery sweep uses.
	ListAll(ctx context.Context) ([]Thread, error)
	SetStatus(ctx context.Context, id string, params SetStatusParams) (Thread, error)
	SetSession(ctx context.Context, id, sessionID string) (Thread, error)
	Delete(ctx context.Context, id string) error
}
