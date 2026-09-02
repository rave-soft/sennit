package thread

import (
	"context"
	"errors"
)

// ErrNameTaken is part of the Store contract: Create must return an error
// wrapping ErrNameTaken (so callers can find it with errors.Is) whenever
// the insert failed because (project_path, kind, name) is already in
// use. This package must not import internal/db, so it cannot itself
// tell a unique-constraint violation apart from any other write failure
// — that recognition has to happen in the concrete implementation, which
// does know its storage's error shapes, and the result is reported back
// through this sentinel rather than through a driver-specific type or a
// message text comparison.
var ErrNameTaken = errors.New("thread: name already in use")

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
	// ParentSessionID is the session this delegation's own session nests
	// under; see [Delegation.ParentSessionID]. It is persisted, and so
	// survives a process restart — unlike threadControl.parentSessionID,
	// which caches the same value in memory for admission checks only.
	// Empty means no parent.
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

// FinalizeTaskParams is the immutable terminal payload committed together
// with cost attribution and the durable completion outbox.
type FinalizeTaskParams struct {
	Status          Status
	Error           string
	ResultSummary   string
	CompletedAt     int64
	CompletionDepth int
	TerminalAt      int64
}

// Store persists threads. Get and GetByName return an error wrapping
// ErrNotFound when no matching row exists. It is a thin persistence contract with no
// pub/sub of its own — the thread manager owns lifecycle events built on
// top of these operations. The sqlc-backed implementation lives in
// internal/app/threadspawn (NewStore): this domain package declares the
// contract and must not import internal/db.
type Store interface {
	// Create must return an error wrapping ErrNameTaken when the insert
	// violates the (project_path, kind, name) uniqueness constraint — see
	// ErrNameTaken's doc comment.
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

// TaskFinalizationStore is the transactional extension required by task
// lifecycle finalization. It is separate from Store so lightweight thread
// stores and test doubles that never finalize tasks remain valid.
type TaskFinalizationStore interface {
	FinalizeTask(ctx context.Context, id string, params FinalizeTaskParams) (st Thread, finalized bool, err error)
	ListPendingTaskCompletions(ctx context.Context) ([]Thread, error)
	MarkTaskCompletionDelivered(ctx context.Context, id string) error
}
