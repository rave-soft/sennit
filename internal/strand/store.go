package strand

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/db"
)

// CreateParams holds the fields needed to create a new strand. Status
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

// Store persists strands. It is a thin wrapper around the generated sqlc
// queries with no pub/sub of its own — the strand manager owns lifecycle
// events built on top of these operations.
type Store interface {
	Create(ctx context.Context, params CreateParams) (Strand, error)
	Get(ctx context.Context, id string) (Strand, error)
	GetByName(ctx context.Context, name string) (Strand, error)
	List(ctx context.Context) ([]Strand, error)
	SetStatus(ctx context.Context, id string, params SetStatusParams) (Strand, error)
	SetSession(ctx context.Context, id, sessionID string) (Strand, error)
	Delete(ctx context.Context, id string) error
}

type store struct {
	q db.Querier
}

// NewStore returns a Store backed by the given sqlc queries.
func NewStore(q db.Querier) Store {
	return &store{q: q}
}

func (s *store) Create(ctx context.Context, params CreateParams) (Strand, error) {
	mergePolicy := params.MergePolicy
	if mergePolicy == "" {
		mergePolicy = MergeAuto
	}

	dbStrand, err := s.q.CreateStrand(ctx, db.CreateStrandParams{
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
		return Strand{}, err
	}
	return fromDBItem(dbStrand), nil
}

func (s *store) Get(ctx context.Context, id string) (Strand, error) {
	dbStrand, err := s.q.GetStrand(ctx, id)
	if err != nil {
		return Strand{}, err
	}
	return fromDBItem(dbStrand), nil
}

func (s *store) GetByName(ctx context.Context, name string) (Strand, error) {
	dbStrand, err := s.q.GetStrandByName(ctx, name)
	if err != nil {
		return Strand{}, err
	}
	return fromDBItem(dbStrand), nil
}

func (s *store) List(ctx context.Context) ([]Strand, error) {
	dbStrands, err := s.q.ListStrands(ctx)
	if err != nil {
		return nil, err
	}
	strands := make([]Strand, len(dbStrands))
	for i, dbStrand := range dbStrands {
		strands[i] = fromDBItem(dbStrand)
	}
	return strands, nil
}

func (s *store) SetStatus(ctx context.Context, id string, params SetStatusParams) (Strand, error) {
	dbStrand, err := s.q.UpdateStrandStatus(ctx, db.UpdateStrandStatusParams{
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
		return Strand{}, err
	}
	return fromDBItem(dbStrand), nil
}

func (s *store) SetSession(ctx context.Context, id, sessionID string) (Strand, error) {
	dbStrand, err := s.q.UpdateStrandSession(ctx, db.UpdateStrandSessionParams{
		ID:        id,
		SessionID: sessionID,
	})
	if err != nil {
		return Strand{}, err
	}
	return fromDBItem(dbStrand), nil
}

func (s *store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteStrand(ctx, id)
}

func fromDBItem(item db.Strand) Strand {
	return Strand{
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
