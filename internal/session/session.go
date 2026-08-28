package session

import (
	"errors"
	"fmt"

	"github.com/zeebo/xxh3"
)

// ErrNotFound is returned by [store.Service.Get] when no session has the
// requested id — a session deleted or never created, rather than a
// failure to reach the store. It lives on the domain model, not the
// store package, because callers as far out as internal/ui classify
// errors against it without needing anything else the store offers.
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
	// [store.Service.ListSubAgentSessions].
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
