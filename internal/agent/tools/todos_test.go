package tools

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeTodoSessions implements just enough of session.Service for the todos
// tool: Get returns whatever was last Saved (or the seeded session), Save
// records the write.
type fakeTodoSessions struct {
	session.Service
	sess session.Session
}

func (f *fakeTodoSessions) Get(_ context.Context, id string) (session.Session, error) {
	return f.sess, nil
}

func (f *fakeTodoSessions) Save(_ context.Context, s session.Session) (session.Session, error) {
	f.sess = s
	return s, nil
}

// runTodosTool invokes the todos tool with the given items and returns the
// saved session's todos plus the response metadata.
func runTodosTool(t *testing.T, sessions *fakeTodoSessions, items []TodoItem) ([]session.Todo, TodosResponseMetadata) {
	t.Helper()
	tool := NewTodosTool(sessions)

	input, err := json.Marshal(TodosParams{Todos: items})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta TodosResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))

	return sessions.sess.Todos, meta
}

// TestTodosTool_PartialUpdateCarriesOverCompletedItems is the regression
// test for completed todos silently vanishing: a model that omits
// previously-completed items from its latest todos call must not cause them
// to disappear from the stored session.
func TestTodosTool_PartialUpdateCarriesOverCompletedItems(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
			{Content: "ship it", Status: session.TodoStatusPending},
		},
	}}

	// The model only mentions the pending item this time, omitting the
	// already-completed "write tests".
	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "ship it", Status: "in_progress", ActiveForm: "Shipping it"},
	})

	require.Len(t, todos, 2)
	require.Equal(t, "ship it", todos[0].Content, "incoming item stays first")
	require.Equal(t, session.TodoStatusInProgress, todos[0].Status)
	require.Equal(t, "write tests", todos[1].Content, "completed item carried over at the tail")
	require.Equal(t, session.TodoStatusCompleted, todos[1].Status)

	require.Equal(t, 1, meta.Completed, "carried-over completed item counts toward Completed")
	require.Equal(t, 2, meta.Total)
}

// TestTodosTool_EmptyUpdateIsExplicitReset covers that an empty params.Todos
// is treated as a deliberate clear, not a partial update — no merge-back.
func TestTodosTool_EmptyUpdateIsExplicitReset(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
			{Content: "ship it", Status: session.TodoStatusPending},
		},
	}}

	todos, meta := runTodosTool(t, sessions, nil)

	require.Empty(t, todos)
	require.Equal(t, 0, meta.Total)
	require.Equal(t, 0, meta.Completed)
}

// TestTodosTool_ReappearingCompletedItemIsNotDuplicated covers that an old
// completed todo re-sent by the model (verbatim, or with a different
// status) appears exactly once in the result, with the incoming fields
// winning over the stored ones.
func TestTodosTool_ReappearingCompletedItemIsNotDuplicated(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
		},
	}}

	// Re-sent with a different status (e.g. the model reopens it).
	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "write tests", Status: "pending"},
	})

	require.Len(t, todos, 1, "must not duplicate the reappearing item")
	require.Equal(t, session.TodoStatusPending, todos[0].Status, "incoming status wins")
	require.Equal(t, 0, meta.Completed)
	require.Equal(t, 1, meta.Total)
}

// TestTodosTool_MergeNormalizesWhitespaceAndCase covers that content
// matching for merge-back purposes ignores surrounding whitespace and case
// differences.
func TestTodosTool_MergeNormalizesWhitespaceAndCase(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "  Write Tests  ", Status: session.TodoStatusCompleted},
		},
	}}

	// Incoming item matches only after trim + case-fold.
	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "write tests", Status: "completed"},
	})

	require.Len(t, todos, 1, "normalized match must not carry over a duplicate")
	require.Equal(t, "write tests", todos[0].Content, "incoming content wins")
	require.Equal(t, 1, meta.Completed)
	require.Equal(t, 1, meta.Total)
}

// TestTodosTool_DedupesDuplicateOldCompletedEntries is a defensive test:
// even if the stored session somehow has two completed todos with the same
// normalized content, the merge-back must not duplicate them in the output.
func TestTodosTool_DedupesDuplicateOldCompletedEntries(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
			{Content: "Write Tests", Status: session.TodoStatusCompleted},
			{Content: "ship it", Status: session.TodoStatusPending},
		},
	}}

	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "ship it", Status: "pending"},
	})

	require.Len(t, todos, 2, "duplicate old completed entries collapse to one")
	require.Equal(t, "ship it", todos[0].Content)
	require.Equal(t, "write tests", todos[1].Content)
	require.Equal(t, 1, meta.Completed)
	require.Equal(t, 2, meta.Total)
}

func TestTodosTool_StartsNewCycleAfterAllTodosCompleted(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
			{Content: "ship it", Status: session.TodoStatusCompleted},
		},
	}}

	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "monitor rollout", Status: "in_progress", ActiveForm: "Monitoring rollout"},
	})

	require.Equal(t, []session.Todo{{
		Content:    "monitor rollout",
		Status:     session.TodoStatusInProgress,
		ActiveForm: "Monitoring rollout",
	}}, todos)
	require.True(t, meta.IsNew)
	require.Equal(t, 0, meta.Completed)
	require.Equal(t, 1, meta.Total)
}

func TestTodosTool_RepeatedCompletedListDoesNotStartNewCycle(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
		},
	}}

	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "write tests", Status: "completed"},
	})

	require.Len(t, todos, 1)
	require.False(t, meta.IsNew)
	require.Equal(t, 1, meta.Completed)
}

// TestTodosTool_InvalidStatusIsTextResponseNotError pins an error-vs-response
// fix: an invalid status is bad input the model supplied, so it is a normal
// (IsError) tool result the model can see and correct, not a Go error that
// aborts the whole tool-call batch.
func TestTodosTool_InvalidStatusIsTextResponseNotError(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{ID: "session-1"}}
	tool := NewTodosTool(sessions)

	input, err := json.Marshal(TodosParams{Todos: []TodoItem{
		{Content: "write tests", Status: "in-progress"},
	}})
	require.NoError(t, err)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session-1")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, `invalid status "in-progress" for todo "write tests"`)
}
