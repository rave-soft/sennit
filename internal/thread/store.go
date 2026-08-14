package thread

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/db"
)

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

// Store persists threads. It is a thin wrapper around the generated sqlc
// queries with no pub/sub of its own — the thread manager owns lifecycle
// events built on top of these operations.
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

type store struct {
	q           db.Querier
	projectPath string
}

// NewStore returns a Store backed by the given sqlc queries, scoped to
// projectPath: threads now live in a single shared database, so names
// and listings are scoped per project to keep each project's threads
// isolated from every other project's.
func NewStore(q db.Querier, projectPath string) Store {
	return &store{q: q, projectPath: projectPath}
}

func (s *store) Create(ctx context.Context, params CreateParams) (Thread, error) {
	kind := params.Kind
	if kind == "" {
		kind = KindThread
	}
	// MergePolicy is a Thread-overlay concept: defaulting it for every
	// kind would leave a non-thread row reading MergeAuto, which used to
	// be enough on its own to send a task into Manager's merge flow (see
	// onAutoMerge, which now also guards on Kind directly). Only a thread
	// gets the default; every other kind's column stays "".
	mergePolicy := params.MergePolicy
	if mergePolicy == "" && kind == KindThread {
		mergePolicy = MergeAuto
	}

	dbThread, err := s.q.CreateThread(ctx, db.CreateThreadParams{
		ID:           uuid.New().String(),
		Name:         params.Name,
		ProjectPath:  s.projectPath,
		Goal:         params.Goal,
		BaseBranch:   params.BaseBranch,
		Branch:       params.Branch,
		WorktreePath: params.WorktreePath,
		SessionID:    params.SessionID,
		Status:       string(StatusPending),
		MergePolicy:  string(mergePolicy),
		Kind:         string(kind),
	})
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) Get(ctx context.Context, id string) (Thread, error) {
	dbThread, err := s.q.GetThread(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) GetByName(ctx context.Context, name string) (Thread, error) {
	dbThread, err := s.q.GetThreadByName(ctx, db.GetThreadByNameParams{
		Name:        name,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) List(ctx context.Context) ([]Thread, error) {
	dbThreads, err := s.q.ListThreads(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, len(dbThreads))
	for i, dbThread := range dbThreads {
		threads[i] = fromDBItem(dbThread)
	}
	return threads, nil
}

func (s *store) ListAll(ctx context.Context) ([]Thread, error) {
	dbThreads, err := s.q.ListThreadsAll(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	threads := make([]Thread, len(dbThreads))
	for i, dbThread := range dbThreads {
		threads[i] = fromDBItem(dbThread)
	}
	return threads, nil
}

func (s *store) SetStatus(ctx context.Context, id string, params SetStatusParams) (Thread, error) {
	dbThread, err := s.q.UpdateThreadStatus(ctx, db.UpdateThreadStatusParams{
		ID:            id,
		Status:        string(params.Status),
		Error:         params.Error,
		ResultSummary: params.ResultSummary,
		CompletedAt: sql.NullInt64{
			Int64: params.CompletedAt,
			Valid: params.CompletedAt != 0,
		},
	})
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) SetSession(ctx context.Context, id, sessionID string) (Thread, error) {
	dbThread, err := s.q.UpdateThreadSession(ctx, db.UpdateThreadSessionParams{
		ID:        id,
		SessionID: sessionID,
	})
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteThread(ctx, id)
}

func fromDBItem(item db.Thread) Thread {
	return Thread{
		Delegation: Delegation{
			ID:            item.ID,
			Name:          item.Name,
			Goal:          item.Goal,
			SessionID:     item.SessionID,
			Status:        Status(item.Status),
			Kind:          Kind(item.Kind),
			ResultSummary: item.ResultSummary,
			Error:         item.Error,
			CreatedAt:     item.CreatedAt,
			UpdatedAt:     item.UpdatedAt,
			CompletedAt:   item.CompletedAt.Int64,
		},
		BaseBranch:   item.BaseBranch,
		Branch:       item.Branch,
		WorktreePath: item.WorktreePath,
		MergePolicy:  MergePolicy(item.MergePolicy),
	}
}
