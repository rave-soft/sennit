package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
)

type Service interface {
	pubsub.Subscriber[session.Session]
	Create(ctx context.Context, title string) (session.Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (session.Session, error)
	// CreateSubAgentSession creates a delegation's child session and
	// stamps it with the delegating agent's id, which is what later
	// makes [Service.ListSubAgentSessions] able to find it. agentID
	// must be non-empty; use CreateTaskSession for an anonymous
	// delegation.
	CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (session.Session, error)
	// ListSubAgentSessions returns agentID's earlier sessions under
	// parentSessionID, oldest first, excluding excludeSessionID (the
	// session being started now). Returns nothing when agentID is
	// empty rather than matching every anonymous child session.
	ListSubAgentSessions(ctx context.Context, parentSessionID, agentID, excludeSessionID string) ([]session.Session, error)
	// Get returns the session with the given id, or an error wrapping
	// [session.ErrNotFound] when no session has that id.
	Get(ctx context.Context, id string) (session.Session, error)
	GetLast(ctx context.Context) (session.Session, error)
	List(ctx context.Context) ([]session.Session, error)
	ValidateSessionIDsInTree(ctx context.Context, rootSessionID string, sessionIDs []string) ([]string, error)
	// Save writes back sess's whole row, title included. It has no
	// production caller: renaming goes through Rename (a narrow,
	// title-only write), and every other writer here (SaveUsage,
	// SetModel, SetTodos, ...) touches only the columns it owns for the
	// same reason - a wide write like this one collides with whatever
	// else is concurrently updating the row (see SaveUsage's comment
	// below, and G3 in REFACTORING.md, which closed exactly this as a UI
	// write path). Save survives only because tests want a one-call way
	// to fabricate a fully-populated session row; it must not grow a
	// production caller again.
	Save(ctx context.Context, sess session.Session) (session.Session, error)
	// SaveUsage persists sess's token/summary/todo fields the way Save
	// does, but ignores sess.Cost and sess.Title: costDelta is
	// accumulated onto the session's existing cost with a single atomic
	// UPDATE (cost = cost + costDelta) instead of writing back a whole
	// total, and the title column is left untouched entirely. Use this
	// over Save when the caller's read of sess happened long enough ago
	// that another writer (e.g. an async title-generation save landing
	// against this same session) could plausibly have landed in between —
	// summarize's provider stream is the case this exists for. Title is
	// excluded rather than raced the same way cost is because nothing
	// that calls SaveUsage means to rename the session; renaming goes
	// through Rename, not Save (Save has no production caller - see its
	// own doc above).
	SaveUsage(ctx context.Context, sess session.Session, costDelta float64) (session.Session, error)
	UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error
	Rename(ctx context.Context, id string, title string) error
	// SetModel pins the model sessionID runs on. A zero ModelRef clears
	// the pin, returning the session to the instance's own selection.
	//
	// It is called from the dispatch path on every turn, so it is a bare
	// write: no row is read back and no event is published. Nothing
	// renders a session's pinned model, and republishing the session on
	// every turn would wake every subscriber for a field none of them
	// display.
	SetModel(ctx context.Context, sessionID string, model session.ModelRef) error
	// SetTodos writes only sessionID's todo list: the todo tool runs
	// mid-turn, alongside the turn's own usage saves, and a full-row
	// write from either side would carry a stale copy of what the other
	// just wrote.
	SetTodos(ctx context.Context, sessionID string, todos []session.Todo) error
	Delete(ctx context.Context, id string) error
	// DescendantCost sums cost over every session nested under
	// sessionID, at any depth, excluding sessionID's own row. A session
	// with no delegations legitimately sums to 0, not
	// [session.ErrNotFound].
	DescendantCost(ctx context.Context, sessionID string) (float64, error)
}

// TelemetrySink is the narrow seam the session service reports its
// lifecycle through, so internal/session/store never reaches into the
// telemetry package. The composition layer (app and the session CLI
// commands) wires it to the telemetry sink; a service built without one
// simply does not report.
type TelemetrySink interface {
	SessionCreated()
	SessionDeleted()
}

type service struct {
	*pubsub.Broker[session.Session]
	db          *sql.DB
	q           *db.Queries
	projectPath string

	// telemetry receives the service's lifecycle reports; nil when no
	// composition wired a sink (the zero service built in tests).
	telemetry TelemetrySink

	// Estimated usage stays in memory so fetch-modify-save paths (e.g.,
	// updating todos) do not rebuild a session from SQLite and
	// incorrectly clear the UI "~" marker.
	estimatedUsageMu sync.RWMutex
	estimatedUsage   map[string]bool
}

func (s *service) Create(ctx context.Context, title string) (session.Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:          uuid.New().String(),
		Title:       title,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return session.Session{}, err
	}
	sess := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, sess)
	s.reportSessionCreated()
	return sess, nil
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (session.Session, error) {
	return s.CreateSubAgentSession(ctx, toolCallID, parentSessionID, title, "")
}

func (s *service) CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (session.Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
		ProjectPath:     s.projectPath,
		AgentID:         agentID,
	})
	if err != nil {
		return session.Session{}, err
	}
	sess := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, sess)
	return sess, nil
}

func (s *service) ListSubAgentSessions(ctx context.Context, parentSessionID, agentID, excludeSessionID string) ([]session.Session, error) {
	// The SQL guards against this too, but an empty agentID reaching the
	// database at all means a caller lost track of which delegations are
	// named; refuse it here where the mistake is still legible.
	if agentID == "" {
		return nil, nil
	}
	dbSessions, err := s.q.ListSubAgentSessions(ctx, db.ListSubAgentSessionsParams{
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		AgentID:         agentID,
		ID:              excludeSessionID,
	})
	if err != nil {
		return nil, err
	}
	sessions := make([]session.Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
		s.applyEstimatedUsageState(&sessions[i])
	}
	return sessions, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	var dbSession db.Session
	var treeIDs []string
	var deleted []db.Session
	err := db.InTx(ctx, s.db, func(qtx *db.Queries) error {
		var err error
		dbSession, err = qtx.GetSessionByID(ctx, id)
		if err != nil {
			return err
		}
		// parent_session_id carries no foreign key, so nothing cascades
		// from a session to its sub-sessions. Adding one would mean
		// rebuilding the sessions table, and renaming it away with
		// foreign keys on rewrites the references in messages, files
		// and read_files to point at the renamed copy; the usual
		// PRAGMA foreign_keys = OFF around that is silently ignored
		// inside the transaction goose runs a migration in. So the
		// subtree is deleted here instead: without it every agent-tool
		// and title session under this one is left orphaned, invisible
		// to the UI and reachable only by `sennit gc`.
		treeIDs, err = qtx.ListSessionTreeIDs(ctx, dbSession.ID)
		if err != nil {
			return fmt.Errorf("listing session tree: %w", err)
		}
		for _, treeID := range treeIDs {
			// Read the row before deleting it: every row in the subtree
			// gets its own DeletedEvent below, and a subscriber that is
			// looking at one of them needs to recognise it by more than
			// an id. The root is already in hand from GetSessionByID.
			if treeID != dbSession.ID {
				child, getErr := qtx.GetSessionByID(ctx, treeID)
				if getErr != nil {
					return fmt.Errorf("reading session %s before deleting it: %w", treeID, getErr)
				}
				deleted = append(deleted, child)
			}
			// Messages, files and read_files go with each row through
			// their own ON DELETE CASCADE.
			if err = qtx.DeleteSession(ctx, treeID); err != nil {
				return fmt.Errorf("deleting session %s: %w", treeID, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, treeID := range treeIDs {
		s.clearEstimatedUsageState(treeID)
	}
	// One event per deleted row, descendants before the root. Publishing
	// only the root said "this session is gone" while leaving every
	// delegation and title session under it looking alive to anyone
	// holding one - and someone does: the UI puts a delegation in
	// sess.current when the person steps into a sub-agent, compares the
	// event's id against it, misses, and goes on sending turns to a row
	// that no longer exists. Descendants first so a subscriber that
	// reacts to its own session vanishing has already been told before
	// the root's event arrives.
	for _, child := range deleted {
		s.Publish(pubsub.DeletedEvent, s.fromDBItem(child))
	}
	s.Publish(pubsub.DeletedEvent, s.fromDBItem(dbSession))
	s.reportSessionDeleted()
	return nil
}

func (s *service) Get(ctx context.Context, id string) (session.Session, error) {
	dbSession, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The bare driver error ("sql: no rows in result set")
			// says nothing about what was being looked up, and it
			// reaches the user verbatim as a status-line error.
			return session.Session{}, fmt.Errorf("%w: %q", session.ErrNotFound, id)
		}
		return session.Session{}, err
	}
	sess := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&sess)
	return sess, nil
}

func (s *service) GetLast(ctx context.Context) (session.Session, error) {
	dbSession, err := s.q.GetLastSession(ctx, s.projectPath)
	if err != nil {
		return session.Session{}, err
	}
	sess := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&sess)
	return sess, nil
}

func (s *service) Save(ctx context.Context, sess session.Session) (session.Session, error) {
	todosJSON, err := marshalTodos(sess.Todos)
	if err != nil {
		return session.Session{}, err
	}

	dbSession, err := s.q.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               sess.ID,
		Title:            sess.Title,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: sess.SummaryMessageID,
			Valid:  sess.SummaryMessageID != "",
		},
		Cost: sess.Cost,
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
	})
	if err != nil {
		return session.Session{}, err
	}
	estimatedUsage := sess.EstimatedUsage
	s.setEstimatedUsageState(sess.ID, estimatedUsage)
	sess = s.fromDBItem(dbSession)
	sess.EstimatedUsage = estimatedUsage
	s.Publish(pubsub.UpdatedEvent, sess)
	return sess, nil
}

// SaveUsage folds costDelta onto sess's cost with a single atomic UPDATE
// rather than writing back sess.Cost as a whole total. See the interface.
func (s *service) SaveUsage(ctx context.Context, sess session.Session, costDelta float64) (session.Session, error) {
	todosJSON, err := marshalTodos(sess.Todos)
	if err != nil {
		return session.Session{}, err
	}

	dbSession, err := s.q.UpdateSessionUsage(ctx, db.UpdateSessionUsageParams{
		ID:               sess.ID,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: sess.SummaryMessageID,
			Valid:  sess.SummaryMessageID != "",
		},
		Cost: costDelta,
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
	})
	if err != nil {
		return session.Session{}, err
	}
	estimatedUsage := sess.EstimatedUsage
	s.setEstimatedUsageState(sess.ID, estimatedUsage)
	result := s.fromDBItem(dbSession)
	result.EstimatedUsage = estimatedUsage
	s.Publish(pubsub.UpdatedEvent, result)
	return result, nil
}

// SetTodos writes the session's todo list and nothing else. See the
// interface.
func (s *service) SetTodos(ctx context.Context, sessionID string, todos []session.Todo) error {
	todosJSON, err := marshalTodos(todos)
	if err != nil {
		return err
	}
	rows, err := s.q.SetSessionTodos(ctx, db.SetSessionTodosParams{
		Todos: sql.NullString{String: todosJSON, Valid: todosJSON != ""},
		ID:    sessionID,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %q: %w", sessionID, session.ErrNotFound)
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// UpdateTitleAndUsage updates only the title and usage fields atomically.
// This is safer than fetching, modifying, and saving the entire session.
func (s *service) UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error {
	rows, err := s.q.UpdateSessionTitleAndUsage(ctx, db.UpdateSessionTitleAndUsageParams{
		ID:               sessionID,
		Title:            title,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Cost:             cost,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %q: %w", sessionID, session.ErrNotFound)
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// Rename writes only the title column, and only that column: since
// 20260904000000_rename_does_not_bump_session_updated_at the auto-bump
// trigger names the columns it reacts to and title is not among them, so
// a rename leaves updated_at where it was. That matters because
// updated_at is what orders ListSessions, what GetLastSession resumes,
// what ages a session out under `sennit gc`, and what ProjectStatsSince
// subtracts created_at from to report time worked - renaming a year-old
// session should move it in none of those. Usage fields (cost, tokens,
// todos, summary_message_id) are untouched too, which is the guarantee
// this method exists to make.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	rows, err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %q: %w", id, session.ErrNotFound)
	}
	s.publishSessionUpdate(ctx, id)
	return nil
}

func (s *service) SetModel(ctx context.Context, sessionID string, model session.ModelRef) error {
	rows, err := s.q.SetSessionModel(ctx, db.SetSessionModelParams{
		ID:            sessionID,
		ModelProvider: model.Provider,
		ModelID:       model.Model,
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("session %q: %w", sessionID, session.ErrNotFound)
	}
	return nil
}

func (s *service) ValidateSessionIDsInTree(ctx context.Context, rootSessionID string, sessionIDs []string) ([]string, error) {
	if len(sessionIDs) == 0 {
		return nil, nil
	}
	idsJSON, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("marshal session IDs: %w", err)
	}
	return s.q.BatchValidateSessionIDsInTree(ctx, db.BatchValidateSessionIDsInTreeParams{
		SessionIdsJson: string(idsJSON),
		RootSessionID:  rootSessionID,
		ProjectPath:    s.projectPath,
	})
}

// DescendantCost sums cost over sessionID's delegations. See the
// interface.
func (s *service) DescendantCost(ctx context.Context, sessionID string) (float64, error) {
	return s.q.SumDescendantSessionCost(ctx, sessionID)
}

func (s *service) List(ctx context.Context) ([]session.Session, error) {
	dbSessions, err := s.q.ListSessions(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	sessions := make([]session.Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
		s.applyEstimatedUsageState(&sessions[i])
	}
	return sessions, nil
}

// publishSessionUpdate re-fetches a session and publishes an UpdatedEvent so
// that UI subscribers reflect title or usage changes.
func (s *service) publishSessionUpdate(ctx context.Context, sessionID string) {
	sess, err := s.Get(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to re-fetch session for event publish", "error", err, "sessionID", sessionID)
		return
	}
	s.Publish(pubsub.UpdatedEvent, sess)
}

func (s *service) applyEstimatedUsageState(sess *session.Session) {
	s.estimatedUsageMu.RLock()
	sess.EstimatedUsage = s.estimatedUsage[sess.ID]
	s.estimatedUsageMu.RUnlock()
}

func (s *service) setEstimatedUsageState(sessionID string, estimatedUsage bool) {
	s.estimatedUsageMu.Lock()
	defer s.estimatedUsageMu.Unlock()
	if estimatedUsage {
		s.estimatedUsage[sessionID] = true
		return
	}
	delete(s.estimatedUsage, sessionID)
}

func (s *service) clearEstimatedUsageState(sessionID string) {
	s.estimatedUsageMu.Lock()
	delete(s.estimatedUsage, sessionID)
	s.estimatedUsageMu.Unlock()
}

func (s *service) fromDBItem(item db.Session) session.Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	return session.Session{
		ID:               item.ID,
		ParentSessionID:  item.ParentSessionID.String,
		AgentID:          item.AgentID,
		Model:            session.ModelRef{Provider: item.ModelProvider, Model: item.ModelID},
		Title:            item.Title,
		MessageCount:     item.MessageCount,
		PromptTokens:     item.PromptTokens,
		CompletionTokens: item.CompletionTokens,
		SummaryMessageID: item.SummaryMessageID.String,
		Cost:             item.Cost,
		Todos:            todos,
		CreatedAt:        item.CreatedAt,
		UpdatedAt:        item.UpdatedAt,
	}
}

func marshalTodos(todos []session.Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]session.Todo, error) {
	if data == "" {
		return []session.Todo{}, nil
	}
	var todos []session.Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []session.Todo{}, err
	}
	return todos, nil
}

// NewService returns a Service backed by the given sqlc queries, scoped
// to projectPath: sessions now live in a single shared database, so
// "last session" and listings are scoped per project.
func NewService(q *db.Queries, conn *sql.DB, projectPath string, opts ...Option) Service {
	broker := pubsub.NewBroker[session.Session]()
	s := &service{
		Broker:         broker,
		db:             conn,
		q:              q,
		projectPath:    projectPath,
		estimatedUsage: make(map[string]bool),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Option configures a service returned by [NewService].
type Option func(*service)

// WithTelemetry wires the sink the service reports its lifecycle
// (session creation and deletion) through. A service built without it is
// nil-safe and reports nothing.
func WithTelemetry(sink TelemetrySink) Option {
	return func(s *service) { s.telemetry = sink }
}

// reportSessionCreated reports a created session through the wired
// telemetry sink. Nil-safe: a service without a sink reports nothing.
func (s *service) reportSessionCreated() {
	if s.telemetry != nil {
		s.telemetry.SessionCreated()
	}
}

// reportSessionDeleted reports a deleted session through the wired
// telemetry sink. Nil-safe: a service without a sink reports nothing.
func (s *service) reportSessionDeleted() {
	if s.telemetry != nil {
		s.telemetry.SessionDeleted()
	}
}
