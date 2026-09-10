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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/thread"
)

var errFinalizeLost = errors.New("task finalization lost race")

type store struct {
	q           db.Querier
	conn        *sql.DB
	projectPath string
}

// NewStore returns a thread.Store backed by the given sqlc queries,
// scoped to projectPath: threads now live in a single shared database,
// so names and listings are scoped per project to keep each project's
// threads isolated from every other project's.
func NewStore(q db.Querier, projectPath string) thread.Store {
	return &store{q: q, projectPath: projectPath}
}

// NewTransactionalStore returns the production store with access to the
// connection needed to atomically finalize tasks across thread and session rows.
func NewTransactionalStore(conn *sql.DB, projectPath string) thread.Store {
	return &store{q: db.New(conn), conn: conn, projectPath: projectPath}
}

func (s *store) Create(ctx context.Context, params thread.CreateParams) (thread.Thread, error) {
	kind := params.Kind
	if kind == "" {
		kind = thread.KindThread
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
		Kind:            string(kind),
		ParentSessionID: params.ParentSessionID,
		Execution:       params.Execution,
		CompletionDepth: int64(params.Depth),
	})
	if err != nil {
		// Satisfy the thread.Store contract: Create must report a
		// (project_path, kind, name) collision as thread.ErrNameTaken,
		// findable with errors.Is, so thread stays free to guard its own
		// check-then-act race without knowing this store is backed by
		// SQLite at all.
		if db.IsUniqueConstraintError(err) {
			return thread.Thread{}, fmt.Errorf("%w: %w", thread.ErrNameTaken, err)
		}
		return thread.Thread{}, err
	}
	return fromDBItem(dbThread), nil
}

func (s *store) Get(ctx context.Context, id string) (thread.Thread, error) {
	dbThread, err := s.q.GetThread(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return thread.Thread{}, fmt.Errorf("%w: %q", thread.ErrNotFound, id)
	}
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
	if errors.Is(err, sql.ErrNoRows) {
		return thread.Thread{}, fmt.Errorf("%w: %q", thread.ErrNotFound, name)
	}
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
		threads[i] = fromListRow(db.ListThreadsAllRow(dbThread))
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
		threads[i] = fromListRow(dbThread)
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

func (s *store) SetTaskPreparation(ctx context.Context, id, base, branch, path string) (thread.Thread, error) {
	item, err := s.q.SetTaskPreparation(ctx, db.SetTaskPreparationParams{
		ID: id, BaseBranch: base, Branch: branch, WorktreePath: path,
	})
	if err != nil {
		return thread.Thread{}, err
	}
	return fromDBItem(item), nil
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

func (s *store) FinalizeTask(ctx context.Context, id string, params thread.FinalizeTaskParams) (thread.Thread, bool, error) {
	if s.conn == nil {
		return thread.Thread{}, false, errors.New("thread store does not support transactions")
	}
	var finalized db.Thread
	err := db.InTx(ctx, s.conn, func(q *db.Queries) error {
		st, err := q.GetThread(ctx, id)
		if err != nil {
			return fmt.Errorf("load task for finalization: %w", err)
		}
		if st.Kind != string(thread.KindTask) || (st.Status != string(thread.StatusRunning) && st.Status != string(thread.StatusPending)) {
			return nil
		}
		if _, err := q.AttributeTaskCostOnce(ctx, db.AttributeTaskCostOnceParams{
			ID: st.ID, SessionID: st.SessionID, ParentSessionID: st.ParentSessionID,
		}); err != nil {
			return err
		}
		finalized, err = q.FinalizeTask(ctx, db.FinalizeTaskParams{
			Status: string(params.Status), Error: params.Error, ResultSummary: params.ResultSummary,
			CompletedAt: sql.NullInt64{Int64: params.CompletedAt, Valid: params.CompletedAt != 0},
			TerminalAt:  sql.NullInt64{Int64: params.TerminalAt, Valid: true}, CompletionDepth: int64(params.CompletionDepth),
			ID: st.ID, SessionID: st.SessionID, ParentSessionID: st.ParentSessionID,
		})
		if errors.Is(err, sql.ErrNoRows) {
			return errFinalizeLost
		}
		if err != nil {
			return err
		}
		return q.InsertTaskCompletionOutbox(ctx, db.InsertTaskCompletionOutboxParams{
			TaskID: finalized.ID, TerminalAt: finalized.TerminalAt.Int64,
			Status: finalized.Status, Error: finalized.Error, ResultSummary: finalized.ResultSummary,
			CompletionDepth: finalized.CompletionDepth, CompletedAt: finalized.CompletedAt,
			Name: finalized.Name, Goal: finalized.Goal, SessionID: finalized.SessionID, ParentSessionID: finalized.ParentSessionID,
		})
	})
	if errors.Is(err, errFinalizeLost) {
		st, getErr := s.Get(ctx, id)
		return st, false, getErr
	}
	if err != nil {
		return thread.Thread{}, false, err
	}
	if finalized.ID == "" {
		st, err := s.Get(ctx, id)
		return st, false, err
	}
	return fromDBItem(finalized), true, nil
}

func (s *store) ListPendingTaskCompletions(ctx context.Context) ([]thread.Thread, error) {
	rows, err := s.q.ListPendingTaskCompletions(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	out := make([]thread.Thread, len(rows))
	for i, row := range rows {
		out[i] = fromPendingDBRow(row)
	}
	return out, nil
}

func (s *store) AcknowledgeTaskCompletionGeneration(ctx context.Context, id string, terminalAt int64) error {
	if s.conn == nil {
		return errors.New("thread store does not support transactions")
	}
	return db.InTx(ctx, s.conn, func(q *db.Queries) error {
		if _, err := q.AcknowledgeTaskCompletionGeneration(ctx, db.AcknowledgeTaskCompletionGenerationParams{TaskID: id, TerminalAt: terminalAt}); err != nil {
			return err
		}
		_, err := q.RefreshTaskCompletionPending(ctx, id)
		return err
	})
}

func fromPendingDBRow(item db.ListPendingTaskCompletionsRow) thread.Thread {
	return fromDBItem(db.Thread{
		ID: item.ID, Name: item.Name_2, ProjectPath: item.ProjectPath, Goal: item.Goal_2,
		BaseBranch: item.BaseBranch, Branch: item.Branch, WorktreePath: item.WorktreePath,
		SessionID: item.SessionID_2, Status: item.Status_2,
		ResultSummary: item.ResultSummary_2, Error: item.Error_2, CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt, CompletedAt: item.CompletedAt_2, Kind: item.Kind,
		ParentSessionID: item.ParentSessionID_2, CompletionPending: 1,
		CompletionDepth: item.CompletionDepth_2, TerminalAt: sql.NullInt64{Int64: item.TerminalAt_2, Valid: true},
		CostAttributed: item.CostAttributed, Execution: item.Execution,
	})
}

// fromListRow maps a listing row, which deliberately carries no execution
// snapshot (see the ListThreads queries): the column runs to tens of
// megabytes per row and no list caller reads it. The two listing queries
// select identical columns, so ListThreadsRow converts to ListThreadsAllRow
// directly and only one mapper is needed.
//
// Execution is left zero rather than faked, so a caller that ever does need
// the snapshot gets it from Get, not silently from a listing.
func fromListRow(item db.ListThreadsAllRow) thread.Thread {
	return fromDBItem(db.Thread{
		ID: item.ID, Name: item.Name, ProjectPath: item.ProjectPath, Goal: item.Goal,
		BaseBranch: item.BaseBranch, Branch: item.Branch, WorktreePath: item.WorktreePath,
		SessionID: item.SessionID, Status: item.Status,
		ResultSummary: item.ResultSummary, Error: item.Error, CreatedAt: item.CreatedAt,
		UpdatedAt: item.UpdatedAt, CompletedAt: item.CompletedAt, Kind: item.Kind,
		ParentSessionID: item.ParentSessionID, CompletionPending: item.CompletionPending,
		CompletionDepth: item.CompletionDepth, TerminalAt: item.TerminalAt,
		CostAttributed: item.CostAttributed,
	})
}

func fromDBItem(item db.Thread) thread.Thread {
	return thread.Thread{
		Delegation: thread.Delegation{
			ID:                item.ID,
			Name:              item.Name,
			Goal:              item.Goal,
			SessionID:         item.SessionID,
			Status:            thread.Status(item.Status),
			Kind:              thread.Kind(item.Kind),
			ResultSummary:     item.ResultSummary,
			Error:             item.Error,
			CreatedAt:         item.CreatedAt,
			UpdatedAt:         item.UpdatedAt,
			CompletedAt:       item.CompletedAt.Int64,
			ParentSessionID:   item.ParentSessionID,
			CompletionPending: item.CompletionPending != 0,
			CompletionDepth:   int(item.CompletionDepth),
			TerminalAt:        item.TerminalAt.Int64,
		},
		BaseBranch:   item.BaseBranch,
		Branch:       item.Branch,
		WorktreePath: item.WorktreePath,
		Execution:    item.Execution,
	}
}
