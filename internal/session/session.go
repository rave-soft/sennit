package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/zeebo/xxh3"
)

// ErrNotFound is returned by [Service.Get] when no session has the
// requested id — a session deleted or never created, rather than a
// failure to reach the store.
var ErrNotFound = errors.New("session: no such session")

type TodoStatus string

const (
	TodoStatusPending    TodoStatus = "pending"
	TodoStatusInProgress TodoStatus = "in_progress"
	TodoStatusCompleted  TodoStatus = "completed"
)

// HashID returns the XXH3 hash of a session ID (UUID) as a hex string.
func HashID(id string) string {
	h := xxh3.New()
	_, _ = h.WriteString(id)
	return fmt.Sprintf("%x", h.Sum(nil))
}

type Todo struct {
	Content    string     `json:"content"`
	Status     TodoStatus `json:"status"`
	ActiveForm string     `json:"active_form"`
}

// HasIncompleteTodos returns true if there are any non-completed todos.
func HasIncompleteTodos(todos []Todo) bool {
	for _, todo := range todos {
		if todo.Status != TodoStatusCompleted {
			return true
		}
	}
	return false
}

// ModelRef names a model the way a model has to be named to be found
// again: by provider as well as id, since a model id is only unique
// within its provider. It mirrors config.SelectedModel, spelled here so
// internal/session does not depend on internal/config.
//
// The zero value means "not pinned", which is a real and common state —
// see [Session.Model].
type ModelRef struct {
	Provider string
	Model    string
}

// IsZero reports whether the ref names no model at all.
func (m ModelRef) IsZero() bool { return m.Provider == "" && m.Model == "" }

type Session struct {
	ID              string
	ParentSessionID string
	// Model is the model this session runs on, pinned so that restoring
	// the session restores the model it was working with rather than
	// whatever the instance happens to have selected now. It is stamped
	// from the turn that actually ran (see the agent's dispatch path), so
	// it records what the session did rather than what someone intended.
	//
	// Zero means not pinned, and is normal: a session that has never run
	// has no model to restore, and one restored from before this was
	// recorded may have none either. Callers fall back to the instance's
	// own selection in that case, which is what every session did before
	// this field existed.
	Model ModelRef
	// AgentID names the configured agent whose delegation this session
	// is, and is empty for every session that is not one: top-level
	// sessions, title sessions, thread and task sessions, and the
	// anonymous `agent`/`agentic_fetch` tool's children. It exists so a
	// named agent's successive delegations under one parent can be
	// recognised as turns of a single continuing conversation - see
	// [Service.ListSubAgentSessions].
	AgentID          string
	Title            string
	MessageCount     int64
	PromptTokens     int64
	CompletionTokens int64
	EstimatedUsage   bool
	SummaryMessageID string
	Cost             float64
	Todos            []Todo
	CreatedAt        int64
	UpdatedAt        int64
}

type Service interface {
	pubsub.Subscriber[Session]
	Create(ctx context.Context, title string) (Session, error)
	CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error)
	CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error)
	// CreateSubAgentSession creates a delegation's child session and
	// stamps it with the delegating agent's id, which is what later
	// makes [Service.ListSubAgentSessions] able to find it. agentID
	// must be non-empty; use CreateTaskSession for an anonymous
	// delegation.
	CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (Session, error)
	// ListSubAgentSessions returns agentID's earlier sessions under
	// parentSessionID, oldest first, excluding excludeSessionID (the
	// session being started now). Returns nothing when agentID is
	// empty rather than matching every anonymous child session.
	ListSubAgentSessions(ctx context.Context, parentSessionID, agentID, excludeSessionID string) ([]Session, error)
	// Get returns the session with the given id, or an error wrapping
	// [ErrNotFound] when no session has that id.
	Get(ctx context.Context, id string) (Session, error)
	GetLast(ctx context.Context) (Session, error)
	List(ctx context.Context) ([]Session, error)
	ValidateSessionIDsInTree(ctx context.Context, rootSessionID string, sessionIDs []string) ([]string, error)
	Save(ctx context.Context, session Session) (Session, error)
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
	SetModel(ctx context.Context, sessionID string, model ModelRef) error
	// AddCost accumulates delta onto sessionID's recorded cost with a
	// single narrow UPDATE. It replaces a Get/modify/Save round trip that
	// raced every other writer of the row.
	AddCost(ctx context.Context, sessionID string, delta float64) error
	// SetTodos writes only sessionID's todo list, for the same reason:
	// the todo tool runs mid-turn, alongside the turn's own usage saves.
	SetTodos(ctx context.Context, sessionID string, todos []Todo) error
	Delete(ctx context.Context, id string) error

	// Agent tool session management
	CreateAgentToolSessionID(messageID, toolCallID string) string
	ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool)
	IsAgentToolSession(sessionID string) bool
}

// TelemetrySink is the narrow seam the session service reports its
// lifecycle through, so internal/session never reaches into the telemetry
// package. The composition layer (app and the session CLI commands) wires
// it to the telemetry sink; a service built without one simply does not
// report.
type TelemetrySink interface {
	SessionCreated()
	SessionDeleted()
}

type service struct {
	*pubsub.Broker[Session]
	db          *sql.DB
	q           *db.Queries
	projectPath string

	// telemetry receives the service's lifecycle reports; nil when no
	// composition wired a sink (the zero service built in tests).
	telemetry TelemetrySink

	// Estimated usage stays in memory so fetch-modify-save paths (e.g.,
	// updating todos or parent-session cost) do not rebuild a session from
	// SQLite and incorrectly clear the UI "~" marker.
	estimatedUsageMu sync.RWMutex
	estimatedUsage   map[string]bool
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:          uuid.New().String(),
		Title:       title,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	s.reportSessionCreated()
	return session, nil
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	return s.CreateSubAgentSession(ctx, toolCallID, parentSessionID, title, "")
}

func (s *service) CreateSubAgentSession(ctx context.Context, toolCallID, parentSessionID, title, agentID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
		ProjectPath:     s.projectPath,
		AgentID:         agentID,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) ListSubAgentSessions(ctx context.Context, parentSessionID, agentID, excludeSessionID string) ([]Session, error) {
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
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
		s.applyEstimatedUsageState(&sessions[i])
	}
	return sessions, nil
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
		ProjectPath:     s.projectPath,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	var dbSession db.Session
	var treeIDs []string
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

	session := s.fromDBItem(dbSession)
	for _, treeID := range treeIDs {
		s.clearEstimatedUsageState(treeID)
	}
	s.Publish(pubsub.DeletedEvent, session)
	s.reportSessionDeleted()
	return nil
}

func (s *service) Get(ctx context.Context, id string) (Session, error) {
	dbSession, err := s.q.GetSessionByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The bare driver error ("sql: no rows in result set")
			// says nothing about what was being looked up, and it
			// reaches the user verbatim as a status-line error.
			return Session{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&session)
	return session, nil
}

func (s *service) GetLast(ctx context.Context) (Session, error) {
	dbSession, err := s.q.GetLastSession(ctx, s.projectPath)
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.applyEstimatedUsageState(&session)
	return session, nil
}

func (s *service) Save(ctx context.Context, session Session) (Session, error) {
	todosJSON, err := marshalTodos(session.Todos)
	if err != nil {
		return Session{}, err
	}

	dbSession, err := s.q.UpdateSession(ctx, db.UpdateSessionParams{
		ID:               session.ID,
		Title:            session.Title,
		PromptTokens:     session.PromptTokens,
		CompletionTokens: session.CompletionTokens,
		SummaryMessageID: sql.NullString{
			String: session.SummaryMessageID,
			Valid:  session.SummaryMessageID != "",
		},
		Cost: session.Cost,
		Todos: sql.NullString{
			String: todosJSON,
			Valid:  todosJSON != "",
		},
	})
	if err != nil {
		return Session{}, err
	}
	estimatedUsage := session.EstimatedUsage
	s.setEstimatedUsageState(session.ID, estimatedUsage)
	session = s.fromDBItem(dbSession)
	session.EstimatedUsage = estimatedUsage
	s.Publish(pubsub.UpdatedEvent, session)
	return session, nil
}

// AddCost accumulates delta onto the session's cost. See the interface.
func (s *service) AddCost(ctx context.Context, sessionID string, delta float64) error {
	rows, err := s.q.AddSessionCost(ctx, db.AddSessionCostParams{
		Cost: delta,
		ID:   sessionID,
	})
	if err != nil {
		return err
	}
	// A narrow UPDATE against a row that is not there affects nothing and
	// reports no error, which would turn "the parent session is gone"
	// into a silent success. Callers accumulate a delegation's cost onto
	// its parent and want to know when there is no parent left.
	if rows == 0 {
		return fmt.Errorf("session %q: %w", sessionID, ErrNotFound)
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// SetTodos writes the session's todo list and nothing else. See the
// interface.
func (s *service) SetTodos(ctx context.Context, sessionID string, todos []Todo) error {
	todosJSON, err := marshalTodos(todos)
	if err != nil {
		return err
	}
	if err := s.q.SetSessionTodos(ctx, db.SetSessionTodosParams{
		Todos: sql.NullString{String: todosJSON, Valid: todosJSON != ""},
		ID:    sessionID,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// UpdateTitleAndUsage updates only the title and usage fields atomically.
// This is safer than fetching, modifying, and saving the entire session.
func (s *service) UpdateTitleAndUsage(ctx context.Context, sessionID, title string, promptTokens, completionTokens int64, cost float64) error {
	if err := s.q.UpdateSessionTitleAndUsage(ctx, db.UpdateSessionTitleAndUsageParams{
		ID:               sessionID,
		Title:            title,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		Cost:             cost,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, sessionID)
	return nil
}

// Rename updates only the title of a session without touching updated_at or
// usage fields.
func (s *service) Rename(ctx context.Context, id string, title string) error {
	if err := s.q.RenameSession(ctx, db.RenameSessionParams{
		ID:    id,
		Title: title,
	}); err != nil {
		return err
	}
	s.publishSessionUpdate(ctx, id)
	return nil
}

func (s *service) SetModel(ctx context.Context, sessionID string, model ModelRef) error {
	return s.q.SetSessionModel(ctx, db.SetSessionModelParams{
		ID:            sessionID,
		ModelProvider: model.Provider,
		ModelID:       model.Model,
	})
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

func (s *service) List(ctx context.Context) ([]Session, error) {
	dbSessions, err := s.q.ListSessions(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	sessions := make([]Session, len(dbSessions))
	for i, dbSession := range dbSessions {
		sessions[i] = s.fromDBItem(dbSession)
		s.applyEstimatedUsageState(&sessions[i])
	}
	return sessions, nil
}

// publishSessionUpdate re-fetches a session and publishes an UpdatedEvent so
// that UI subscribers reflect title or usage changes.
func (s *service) publishSessionUpdate(ctx context.Context, sessionID string) {
	session, err := s.Get(ctx, sessionID)
	if err != nil {
		slog.Error("Failed to re-fetch session for event publish", "error", err, "sessionID", sessionID)
		return
	}
	s.Publish(pubsub.UpdatedEvent, session)
}

func (s *service) applyEstimatedUsageState(session *Session) {
	s.estimatedUsageMu.RLock()
	session.EstimatedUsage = s.estimatedUsage[session.ID]
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

func (s *service) fromDBItem(item db.Session) Session {
	todos, err := unmarshalTodos(item.Todos.String)
	if err != nil {
		slog.Error("Failed to unmarshal todos", "session_id", item.ID, "error", err)
	}
	return Session{
		ID:               item.ID,
		ParentSessionID:  item.ParentSessionID.String,
		AgentID:          item.AgentID,
		Model:            ModelRef{Provider: item.ModelProvider, Model: item.ModelID},
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

func marshalTodos(todos []Todo) (string, error) {
	if len(todos) == 0 {
		return "", nil
	}
	data, err := json.Marshal(todos)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func unmarshalTodos(data string) ([]Todo, error) {
	if data == "" {
		return []Todo{}, nil
	}
	var todos []Todo
	if err := json.Unmarshal([]byte(data), &todos); err != nil {
		return []Todo{}, err
	}
	return todos, nil
}

// NewService returns a Service backed by the given sqlc queries, scoped
// to projectPath: sessions now live in a single shared database, so
// "last session" and listings are scoped per project.
func NewService(q *db.Queries, conn *sql.DB, projectPath string, opts ...Option) Service {
	broker := pubsub.NewBroker[Session]()
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

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}
