package stats

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rave-soft/sennit/internal/db"
)

// Scope names which slice of recorded usage a [Snapshot] covers. The
// three are not variations on one query: a session's tree is walked by
// parent link with no time bound (a session is however long it is), while
// the project and global scopes are time-windowed sweeps.
type Scope int

const (
	// ScopeSession covers one session and every sub-agent session
	// beneath it, however long ago it started.
	ScopeSession Scope = iota
	// ScopeProject covers one project's sessions within a time window.
	ScopeProject
	// ScopeGlobal covers every project's sessions within a time window,
	// and additionally fills [Snapshot.Projects].
	ScopeGlobal
)

// String names the scope for a tab label or an error message.
func (s Scope) String() string {
	switch s {
	case ScopeSession:
		return "session"
	case ScopeProject:
		return "project"
	case ScopeGlobal:
		return "global"
	default:
		return "unknown"
	}
}

// Request describes one aggregation to run.
type Request struct {
	Scope Scope
	// SessionID is required for ScopeSession and ignored otherwise.
	SessionID string
	// ProjectPath is required for ScopeProject and ignored otherwise.
	ProjectPath string
	// Since bounds the project and global scopes to sessions created at
	// or after this unix timestamp. Zero means unbounded. ScopeSession
	// ignores it: a session's own history is never partially interesting.
	Since int64
	// WithSkills additionally gathers the skill-usage breakdown, which
	// costs a JSON-scanning query over every message in the window and so
	// is opt-in.
	WithSkills bool
}

// Querier is the slice of the generated db API this package needs.
// Declared here, on the consumer side, so a test can drive Gather with a
// stub instead of a live database.
type Querier interface {
	ListSessionTreeSince(ctx context.Context, id string) ([]db.ListSessionTreeSinceRow, error)
	ListSessionTreeAssistantMessages(ctx context.Context, id string) ([]db.ListSessionTreeAssistantMessagesRow, error)
	ListSessionsSinceWithAgent(ctx context.Context, arg db.ListSessionsSinceWithAgentParams) ([]db.ListSessionsSinceWithAgentRow, error)
	ListAssistantMessagesSince(ctx context.Context, arg db.ListAssistantMessagesSinceParams) ([]db.ListAssistantMessagesSinceRow, error)
	ListAllSessionsSince(ctx context.Context, createdAt int64) ([]db.ListAllSessionsSinceRow, error)
	ListAllAssistantMessagesSince(ctx context.Context, createdAt int64) ([]db.ListAllAssistantMessagesSinceRow, error)
	ListDelegationOutcomesSince(ctx context.Context, arg db.ListDelegationOutcomesSinceParams) ([]db.ListDelegationOutcomesSinceRow, error)
	ListAllDelegationOutcomesSince(ctx context.Context, createdAt int64) ([]db.ListAllDelegationOutcomesSinceRow, error)
	ListSkillLoadsSince(ctx context.Context, arg db.ListSkillLoadsSinceParams) ([]db.ListSkillLoadsSinceRow, error)
	ProjectStatsSince(ctx context.Context, createdAt int64) ([]db.ProjectStatsSinceRow, error)
}

var _ Querier = (*db.Queries)(nil)

// Gather runs req's queries and aggregates them into a [Snapshot].
func Gather(ctx context.Context, q Querier, req Request) (Snapshot, error) {
	sessions, messages, delegations, err := fetch(ctx, q, req)
	if err != nil {
		return Snapshot{}, err
	}

	snap := Snapshot{
		Totals:  ComputeTotals(sessions),
		Models:  ComputeModels(sessions, messages, delegations),
		Agents:  ComputeAgents(sessions, delegations),
		Outcome: ComputeOutcome(delegations),
	}

	if req.Scope == ScopeSession {
		// A single session's "sessions count" would always be 1, which
		// says nothing; what a reader wants there is how much work was
		// delegated out of it.
		snap.Totals.Sessions = int64(len(sessions))
	}

	if req.Scope == ScopeGlobal {
		snap.Projects, err = gatherProjects(ctx, q, req.Since)
		if err != nil {
			return Snapshot{}, err
		}
	}

	if req.WithSkills && req.Scope != ScopeSession {
		rows, err := q.ListSkillLoadsSince(ctx, db.ListSkillLoadsSinceParams{
			CreatedAt:   req.Since,
			ProjectPath: req.ProjectPath,
		})
		if err != nil {
			return Snapshot{}, fmt.Errorf("stats: list skill loads: %w", err)
		}
		snap.Skills = ComputeSkills(rows)
	}

	return snap, nil
}

// fetch runs the three per-scope queries. Each scope reads its own row
// types out of the db package and converts them to this package's neutral
// shapes; everything downstream of here is scope-agnostic.
func fetch(ctx context.Context, q Querier, req Request) ([]Session, []Message, []Delegation, error) {
	switch req.Scope {
	case ScopeSession:
		if req.SessionID == "" {
			return nil, nil, nil, fmt.Errorf("stats: session scope needs a session id")
		}
		rows, err := q.ListSessionTreeSince(ctx, req.SessionID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list session tree: %w", err)
		}
		msgRows, err := q.ListSessionTreeAssistantMessages(ctx, req.SessionID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list session tree messages: %w", err)
		}
		sessions := make([]Session, 0, len(rows))
		for _, r := range rows {
			sessions = append(sessions, Session{
				ID:               r.ID,
				ParentID:         r.ParentSessionID.String,
				Title:            r.Title,
				AgentID:          r.AgentID,
				PromptTokens:     r.PromptTokens,
				CompletionTokens: r.CompletionTokens,
				Cost:             r.Cost,
				CreatedAt:        r.CreatedAt,
				UpdatedAt:        r.UpdatedAt,
			})
		}
		messages := make([]Message, 0, len(msgRows))
		for _, r := range msgRows {
			messages = append(messages, Message{
				SessionID:  r.SessionID,
				Model:      r.Model,
				Provider:   r.Provider,
				CreatedAt:  r.CreatedAt,
				FinishedAt: r.FinishedAt,
			})
		}
		// Delegations are looked up by the sessions in the tree rather
		// than by a scope-wide query: a delegation belongs to this
		// session's stats when this session (or one below it) ran it.
		delegations, err := delegationsForSessions(ctx, q, sessions)
		if err != nil {
			return nil, nil, nil, err
		}
		return sessions, messages, delegations, nil

	case ScopeProject:
		rows, err := q.ListSessionsSinceWithAgent(ctx, db.ListSessionsSinceWithAgentParams{
			CreatedAt:   req.Since,
			ProjectPath: req.ProjectPath,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list project sessions: %w", err)
		}
		msgRows, err := q.ListAssistantMessagesSince(ctx, db.ListAssistantMessagesSinceParams{
			CreatedAt:   req.Since,
			ProjectPath: req.ProjectPath,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list project messages: %w", err)
		}
		delRows, err := q.ListDelegationOutcomesSince(ctx, db.ListDelegationOutcomesSinceParams{
			CreatedAt:   req.Since,
			ProjectPath: req.ProjectPath,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list project delegations: %w", err)
		}
		sessions := make([]Session, 0, len(rows))
		for _, r := range rows {
			sessions = append(sessions, Session{
				ID:               r.ID,
				ParentID:         r.ParentSessionID.String,
				Title:            r.Title,
				AgentID:          r.AgentID,
				PromptTokens:     r.PromptTokens,
				CompletionTokens: r.CompletionTokens,
				Cost:             r.Cost,
				CreatedAt:        r.CreatedAt,
				UpdatedAt:        r.UpdatedAt,
			})
		}
		messages := make([]Message, 0, len(msgRows))
		for _, r := range msgRows {
			messages = append(messages, Message{
				SessionID:  r.SessionID,
				Model:      r.Model,
				Provider:   r.Provider,
				CreatedAt:  r.CreatedAt,
				FinishedAt: r.FinishedAt,
			})
		}
		delegations := make([]Delegation, 0, len(delRows))
		for _, r := range delRows {
			delegations = append(delegations, Delegation{
				ID:          r.ID,
				Kind:        r.Kind,
				Status:      r.Status,
				SessionID:   r.SessionID,
				AgentID:     r.AgentID,
				Title:       r.Title,
				CreatedAt:   r.CreatedAt,
				CompletedAt: r.CompletedAt,
			})
		}
		return sessions, messages, delegations, nil

	case ScopeGlobal:
		rows, err := q.ListAllSessionsSince(ctx, req.Since)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list all sessions: %w", err)
		}
		msgRows, err := q.ListAllAssistantMessagesSince(ctx, req.Since)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list all messages: %w", err)
		}
		delRows, err := q.ListAllDelegationOutcomesSince(ctx, req.Since)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stats: list all delegations: %w", err)
		}
		sessions := make([]Session, 0, len(rows))
		for _, r := range rows {
			sessions = append(sessions, Session{
				ID:               r.ID,
				ParentID:         r.ParentSessionID.String,
				Title:            r.Title,
				AgentID:          r.AgentID,
				PromptTokens:     r.PromptTokens,
				CompletionTokens: r.CompletionTokens,
				Cost:             r.Cost,
				CreatedAt:        r.CreatedAt,
				UpdatedAt:        r.UpdatedAt,
			})
		}
		messages := make([]Message, 0, len(msgRows))
		for _, r := range msgRows {
			messages = append(messages, Message{
				SessionID:  r.SessionID,
				Model:      r.Model,
				Provider:   r.Provider,
				CreatedAt:  r.CreatedAt,
				FinishedAt: r.FinishedAt,
			})
		}
		delegations := make([]Delegation, 0, len(delRows))
		for _, r := range delRows {
			delegations = append(delegations, Delegation{
				ID:          r.ID,
				Kind:        r.Kind,
				Status:      r.Status,
				SessionID:   r.SessionID,
				AgentID:     r.AgentID,
				Title:       r.Title,
				CreatedAt:   r.CreatedAt,
				CompletedAt: r.CompletedAt,
			})
		}
		return sessions, messages, delegations, nil

	default:
		return nil, nil, nil, fmt.Errorf("stats: unknown scope %d", req.Scope)
	}
}

// delegationsForSessions narrows the project's delegation rows down to
// the ones run by sessions in the given set. There is no "delegations of
// a session tree" query because a delegation's row does not record which
// session dispatched it, only which session ran it — and that session is
// exactly what the tree walk already found.
func delegationsForSessions(ctx context.Context, q Querier, sessions []Session) ([]Delegation, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	rows, err := q.ListAllDelegationOutcomesSince(ctx, 0)
	if err != nil {
		return nil, fmt.Errorf("stats: list delegations for session: %w", err)
	}
	inTree := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		inTree[s.ID] = true
	}
	var out []Delegation
	for _, r := range rows {
		if !inTree[r.SessionID] {
			continue
		}
		out = append(out, Delegation{
			ID:          r.ID,
			Kind:        r.Kind,
			Status:      r.Status,
			SessionID:   r.SessionID,
			AgentID:     r.AgentID,
			Title:       r.Title,
			CreatedAt:   r.CreatedAt,
			CompletedAt: r.CompletedAt,
		})
	}
	return out, nil
}

// gatherProjects aggregates one row per project known to the shared
// database, plus a trailing totals row.
func gatherProjects(ctx context.Context, q Querier, since int64) ([]Project, error) {
	dbRows, err := q.ProjectStatsSince(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("stats: gather project stats: %w", err)
	}

	rows := make([]Project, 0, len(dbRows))
	totals := Project{Path: "TOTAL"}
	for _, r := range dbRows {
		promptTokens, _ := CoerceInt64(r.PromptTokens)
		completionTokens, _ := CoerceInt64(r.CompletionTokens)
		timeSeconds, _ := CoerceInt64(r.TimeSeconds)
		cost, _ := CoerceFloat64(r.Cost)

		row := Project{
			Path:             r.ProjectPath,
			Sessions:         r.Sessions,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			Cost:             cost,
			TimeSeconds:      timeSeconds,
		}
		rows = append(rows, row)

		totals.Sessions += row.Sessions
		totals.PromptTokens += row.PromptTokens
		totals.CompletionTokens += row.CompletionTokens
		totals.Cost += row.Cost
		totals.TimeSeconds += row.TimeSeconds
	}

	// The query already orders by token total; sort again in Go so the
	// order holds regardless of the SQL ORDER BY, matching the sibling
	// breakdowns.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Tokens() > rows[j].Tokens() })
	return append(rows, totals), nil
}

// ComputeSkills converts raw skill-load rows (whose aggregate columns come
// back as interface{} because sqlc cannot infer a static type across the
// json_each/json_extract join) into typed rows.
func ComputeSkills(rows []db.ListSkillLoadsSinceRow) []Skill {
	result := make([]Skill, 0, len(rows))
	for _, r := range rows {
		name, ok := r.SkillName.(string)
		if !ok || name == "" {
			continue
		}
		s := Skill{
			Name:         name,
			LoadCount:    r.LoadCount,
			SessionCount: r.SessionCount,
		}
		if v, ok := CoerceInt64(r.FirstUsedAt); ok {
			s.FirstUsedAt = time.Unix(v, 0).Format(time.RFC3339)
		}
		if v, ok := CoerceInt64(r.LastUsedAt); ok {
			s.LastUsedAt = time.Unix(v, 0).Format(time.RFC3339)
		}
		result = append(result, s)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].LoadCount != result[j].LoadCount {
			return result[i].LoadCount > result[j].LoadCount
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// CoerceInt64 coerces a database/sql driver scan result to int64. SQLite
// aggregates over a JSON-extracted column come back as interface{} with a
// driver-dependent concrete type, so the conversion has to be tolerant
// rather than a single assertion.
func CoerceInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

// CoerceFloat64 is [CoerceInt64] for a floating-point aggregate.
func CoerceFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}
