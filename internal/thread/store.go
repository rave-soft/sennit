package thread

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/db"
)

// CreateParams holds the fields needed to create a new thread. Status
// defaults to StatusPending and MergePolicy defaults to MergeAuto when left
// unset.
type CreateParams struct {
	Name         string
	Goal         string
	BaseBranch   string
	Branch       string
	WorktreePath string
	SessionID    string
	MergePolicy  MergePolicy
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
	List(ctx context.Context) ([]Thread, error)
	SetStatus(ctx context.Context, id string, params SetStatusParams) (Thread, error)
	SetSession(ctx context.Context, id, sessionID string) (Thread, error)
	Delete(ctx context.Context, id string) error
}

type store struct {
	q db.Querier
}

// NewStore returns a Store backed by the given sqlc queries.
func NewStore(q db.Querier) Store {
	return &store{q: q}
}

func (s *store) Create(ctx context.Context, params CreateParams) (Thread, error) {
	mergePolicy := params.MergePolicy
	if mergePolicy == "" {
		mergePolicy = MergeAuto
	}

	dbThread, err := s.q.CreateThread(ctx, db.CreateThreadParams{
		ID:           uuid.New().String(),
		Name:         params.Name,
		Goal:         params.Goal,
		BaseBranch:   params.BaseBranch,
		Branch:       params.Branch,
		WorktreePath: params.WorktreePath,
		SessionID:    params.SessionID,
		Status:       string(StatusPending),
		MergePolicy:  string(mergePolicy),
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
	dbThread, err := s.q.GetThreadByName(ctx, name)
	if err != nil {
		return Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) List(ctx context.Context) ([]Thread, error) {
	dbThreads, err := s.q.ListThreads(ctx)
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
		ID:            item.ID,
		Name:          item.Name,
		Goal:          item.Goal,
		BaseBranch:    item.BaseBranch,
		Branch:        item.Branch,
		WorktreePath:  item.WorktreePath,
		SessionID:     item.SessionID,
		Status:        Status(item.Status),
		MergePolicy:   MergePolicy(item.MergePolicy),
		ResultSummary: item.ResultSummary,
		Error:         item.Error,
		CreatedAt:     item.CreatedAt,
		UpdatedAt:     item.UpdatedAt,
		CompletedAt:   item.CompletedAt.Int64,
	}
}
