// Package gather runs the SQL queries stats aggregation needs and hands
// the results to internal/stats' pure Compute* functions. It exists
// separately from internal/stats so that package can stay free of
// database/sql and internal/db — internal/ui imports internal/stats for
// its data types alone, and linking sqlc through it would drag both
// SQLite drivers along for the ride.
package gather

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/stats"
)

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
	ListLatencyEventsSince(ctx context.Context, arg db.ListLatencyEventsSinceParams) ([]db.ListLatencyEventsSinceRow, error)
	ListAllLatencyEventsSince(ctx context.Context, createdAt int64) ([]db.ListAllLatencyEventsSinceRow, error)
	ProjectStatsSince(ctx context.Context, createdAt int64) ([]db.ProjectStatsSinceRow, error)
}

var _ Querier = (*db.Queries)(nil)

// Gather runs req's queries and aggregates them into a [stats.Snapshot].
func Gather(ctx context.Context, q Querier, req stats.Request) (stats.Snapshot, error) {
	sessions, messages, delegations, err := fetch(ctx, q, req)
	if err != nil {
		return stats.Snapshot{}, err
	}

	snap := stats.Snapshot{
		Totals:  stats.ComputeTotals(sessions),
		Models:  stats.ComputeModels(sessions, messages, delegations),
		Agents:  stats.ComputeAgents(sessions, delegations),
		Outcome: stats.ComputeOutcome(delegations),
	}

	if req.Scope == stats.ScopeSession {
		// A single session's "sessions count" would always be 1, which
		// says nothing; what a reader wants there is how much work was
		// delegated out of it.
		snap.Totals.Sessions = int64(len(sessions))
	}

	if req.Scope == stats.ScopeGlobal {
		snap.Projects, err = gatherProjects(ctx, q, req.Since)
		if err != nil {
			return stats.Snapshot{}, err
		}
	}

	if req.WithSkills && req.Scope != stats.ScopeSession {
		rows, err := q.ListSkillLoadsSince(ctx, db.ListSkillLoadsSinceParams{
			CreatedAt:   req.Since,
			ProjectPath: req.ProjectPath,
		})
		if err != nil {
			return stats.Snapshot{}, fmt.Errorf("stats: list skill loads: %w", err)
		}
		snap.Skills = ComputeSkills(rows)
	}

	if req.WithLatency && req.Scope != stats.ScopeSession {
		events, err := gatherLatency(ctx, q, req)
		if err != nil {
			return stats.Snapshot{}, err
		}
		snap.Latency = stats.ComputeLatency(events)
	}

	return snap, nil
}

// gatherLatency reads the recorded handoff waits for req's scope. Unlike
// the other breakdowns this one has no session-tree form, so it lives
// outside fetch rather than adding a fifth return value nothing else
// wants.
func gatherLatency(ctx context.Context, q Querier, req stats.Request) ([]stats.LatencyEvent, error) {
	if req.Scope == stats.ScopeGlobal {
		rows, err := q.ListAllLatencyEventsSince(ctx, req.Since)
		if err != nil {
			return nil, fmt.Errorf("stats: list all latency events: %w", err)
		}
		events := make([]stats.LatencyEvent, 0, len(rows))
		for _, r := range rows {
			events = append(events, stats.LatencyEvent{Kind: r.Kind, WaitedMS: r.WaitedMs})
		}
		return events, nil
	}
	rows, err := q.ListLatencyEventsSince(ctx, db.ListLatencyEventsSinceParams{
		CreatedAt:   req.Since,
		ProjectPath: req.ProjectPath,
	})
	if err != nil {
		return nil, fmt.Errorf("stats: list project latency events: %w", err)
	}
	events := make([]stats.LatencyEvent, 0, len(rows))
	for _, r := range rows {
		events = append(events, stats.LatencyEvent{Kind: r.Kind, WaitedMS: r.WaitedMs})
	}
	return events, nil
}

// rawSessionRow is the column shape shared by every session-scoped stats
// query. sqlc names it differently per query (ListSessionTreeSinceRow,
// ListSessionsSinceWithAgentRow, ListAllSessionsSinceRow) because each
// backs a distinct SQL statement, but the columns themselves are
// identical, so any of the three converts into this one directly.
type rawSessionRow struct {
	ID               string
	ParentSessionID  sql.NullString
	Title            string
	AgentID          string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
	CreatedAt        int64
	UpdatedAt        int64
}

// sessionsFromRows converts one scope's raw session rows into this
// package's neutral [stats.Session] shape. conv does nothing but the
// row-type conversion at each call site, so the field mapping itself —
// the part that used to be copy-pasted once per scope — is written here
// only once.
func sessionsFromRows[T any](rows []T, conv func(T) rawSessionRow) []stats.Session {
	sessions := make([]stats.Session, 0, len(rows))
	for _, r := range rows {
		raw := conv(r)
		sessions = append(sessions, stats.Session{
			ID:               raw.ID,
			ParentID:         raw.ParentSessionID.String,
			Title:            raw.Title,
			AgentID:          raw.AgentID,
			PromptTokens:     raw.PromptTokens,
			CompletionTokens: raw.CompletionTokens,
			Cost:             raw.Cost,
			CreatedAt:        raw.CreatedAt,
			UpdatedAt:        raw.UpdatedAt,
		})
	}
	return sessions
}

// messagesFromRows is [sessionsFromRows] for assistant-message rows. Here
// the neutral [stats.Message] shape matches the db row column-for-column,
// so conv is always a bare type conversion — the duplication removed is
// the three-line loop, not a field mapping.
func messagesFromRows[T any](rows []T, conv func(T) stats.Message) []stats.Message {
	messages := make([]stats.Message, 0, len(rows))
	for _, r := range rows {
		messages = append(messages, conv(r))
	}
	return messages
}

// delegationsFromRows is [messagesFromRows] for delegation-outcome rows;
// [stats.Delegation] likewise matches its db rows column-for-column.
func delegationsFromRows[T any](rows []T, conv func(T) stats.Delegation) []stats.Delegation {
	delegations := make([]stats.Delegation, 0, len(rows))
	for _, r := range rows {
		delegations = append(delegations, conv(r))
	}
	return delegations
}

// fetch runs the three per-scope queries. Each scope reads its own row
// types out of the db package and converts them to this package's neutral
// shapes; everything downstream of here is scope-agnostic.
func fetch(ctx context.Context, q Querier, req stats.Request) ([]stats.Session, []stats.Message, []stats.Delegation, error) {
	switch req.Scope {
	case stats.ScopeSession:
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
		sessions := sessionsFromRows(rows, func(r db.ListSessionTreeSinceRow) rawSessionRow { return rawSessionRow(r) })
		messages := messagesFromRows(msgRows, func(r db.ListSessionTreeAssistantMessagesRow) stats.Message { return stats.Message(r) })
		// Delegations are looked up by the sessions in the tree rather
		// than by a scope-wide query: a delegation belongs to this
		// session's stats when this session (or one below it) ran it.
		delegations, err := delegationsForSessions(ctx, q, sessions)
		if err != nil {
			return nil, nil, nil, err
		}
		return sessions, messages, delegations, nil

	case stats.ScopeProject:
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
		sessions := sessionsFromRows(rows, func(r db.ListSessionsSinceWithAgentRow) rawSessionRow { return rawSessionRow(r) })
		messages := messagesFromRows(msgRows, func(r db.ListAssistantMessagesSinceRow) stats.Message { return stats.Message(r) })
		delegations := delegationsFromRows(delRows, func(r db.ListDelegationOutcomesSinceRow) stats.Delegation { return stats.Delegation(r) })
		return sessions, messages, delegations, nil

	case stats.ScopeGlobal:
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
		sessions := sessionsFromRows(rows, func(r db.ListAllSessionsSinceRow) rawSessionRow { return rawSessionRow(r) })
		messages := messagesFromRows(msgRows, func(r db.ListAllAssistantMessagesSinceRow) stats.Message { return stats.Message(r) })
		delegations := delegationsFromRows(delRows, func(r db.ListAllDelegationOutcomesSinceRow) stats.Delegation { return stats.Delegation(r) })
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
func delegationsForSessions(ctx context.Context, q Querier, sessions []stats.Session) ([]stats.Delegation, error) {
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
	var out []stats.Delegation
	for _, r := range rows {
		if !inTree[r.SessionID] {
			continue
		}
		out = append(out, stats.Delegation(r))
	}
	return out, nil
}

// gatherProjects aggregates one row per project known to the shared
// database, plus a trailing totals row.
func gatherProjects(ctx context.Context, q Querier, since int64) ([]stats.Project, error) {
	dbRows, err := q.ProjectStatsSince(ctx, since)
	if err != nil {
		return nil, fmt.Errorf("stats: gather project stats: %w", err)
	}

	rows := make([]stats.Project, 0, len(dbRows))
	totals := stats.Project{Path: "TOTAL"}
	for _, r := range dbRows {
		promptTokens, _ := CoerceInt64(r.PromptTokens)
		completionTokens, _ := CoerceInt64(r.CompletionTokens)
		timeSeconds, _ := CoerceInt64(r.TimeSeconds)
		cost, _ := CoerceFloat64(r.Cost)

		row := stats.Project{
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
func ComputeSkills(rows []db.ListSkillLoadsSinceRow) []stats.Skill {
	result := make([]stats.Skill, 0, len(rows))
	for _, r := range rows {
		name, ok := r.SkillName.(string)
		if !ok || name == "" {
			continue
		}
		s := stats.Skill{
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
