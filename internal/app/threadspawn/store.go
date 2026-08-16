// Package threadspawn is the composition seam between the app and the
// thread delegation domain: it holds the app-specific wiring that
// internal/thread must not import — the concrete Store over the shared
// database, the Spawners that bootstrap in-process apps, and Attach,
// which gives a workspace ownership of a thread manager.
//
// The domain interfaces themselves (Store, Spawner, Handle, Workspace)
// stay in internal/thread; this package only supplies the
// app/db-backed implementations of them.
package threadspawn

import (
	"context"
	"database/sql"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/thread"
)

type store struct {
	q           db.Querier
	projectPath string
}

// NewStore returns a thread.Store backed by the given sqlc queries,
// scoped to projectPath: threads now live in a single shared database,
// so names and listings are scoped per project to keep each project's
// threads isolated from every other project's.
func NewStore(q db.Querier, projectPath string) thread.Store {
	return &store{q: q, projectPath: projectPath}
}

func (s *store) Create(ctx context.Context, params thread.CreateParams) (thread.Thread, error) {
	kind := params.Kind
	if kind == "" {
		kind = thread.KindThread
	}
	// MergePolicy is a Thread-overlay concept: defaulting it for every
	// kind would leave a non-thread row reading MergeAuto, which used to
	// be enough on its own to send a task into Manager's merge flow (see
	// onAutoMerge, which now also guards on Kind directly). Only a thread
	// gets the default; every other kind's column stays "".
	mergePolicy := params.MergePolicy
	if mergePolicy == "" && kind == thread.KindThread {
		mergePolicy = thread.MergeAuto
	}

	dbThread, err := s.q.CreateThread(ctx, db.CreateThreadParams{
		ID:              uuid.New().String(),
		Name:            params.Name,
		ProjectPath:     s.projectPath,
		Goal:            params.Goal,
		BaseBranch:      params.BaseBranch,
		Branch:          params.Branch,
		WorktreePath:    params.WorktreePath,
		SessionID:       params.SessionID,
		Status:          string(thread.StatusPending),
		MergePolicy:     string(mergePolicy),
		Kind:            string(kind),
		ParentSessionID: params.ParentSessionID,
	})
	if err != nil {
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) Get(ctx context.Context, id string) (thread.Thread, error) {
	dbThread, err := s.q.GetThread(ctx, id)
	if err != nil {
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) GetByName(ctx context.Context, name string) (thread.Thread, error) {
	dbThread, err := s.q.GetThreadByName(ctx, db.GetThreadByNameParams{
		Name:        name,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) List(ctx context.Context) ([]thread.Thread, error) {
	dbThreads, err := s.q.ListThreads(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	threads := make([]thread.Thread, len(dbThreads))
	for i, dbThread := range dbThreads {
		threads[i] = fromDBItem(dbThread)
	}
	return threads, nil
}

func (s *store) ListAll(ctx context.Context) ([]thread.Thread, error) {
	dbThreads, err := s.q.ListThreadsAll(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	threads := make([]thread.Thread, len(dbThreads))
	for i, dbThread := range dbThreads {
		threads[i] = fromDBItem(dbThread)
	}
	return threads, nil
}

func (s *store) SetStatus(ctx context.Context, id string, params thread.SetStatusParams) (thread.Thread, error) {
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
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) SetSession(ctx context.Context, id, sessionID string) (thread.Thread, error) {
	dbThread, err := s.q.UpdateThreadSession(ctx, db.UpdateThreadSessionParams{
		ID:        id,
		SessionID: sessionID,
	})
	if err != nil {
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteThread(ctx, id)
}

func fromDBItem(item db.Thread) thread.Thread {
	return thread.Thread{
		Delegation: thread.Delegation{
			ID:              item.ID,
			Name:            item.Name,
			Goal:            item.Goal,
			SessionID:       item.SessionID,
			Status:          thread.Status(item.Status),
			Kind:            thread.Kind(item.Kind),
			ResultSummary:   item.ResultSummary,
			Error:           item.Error,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
			CompletedAt:     item.CompletedAt.Int64,
			ParentSessionID: item.ParentSessionID,
		},
		BaseBranch:   item.BaseBranch,
		Branch:       item.Branch,
		WorktreePath: item.WorktreePath,
		MergePolicy:  thread.MergePolicy(item.MergePolicy),
	}
}
