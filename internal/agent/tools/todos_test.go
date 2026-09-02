package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeTodoSessions implements the narrow todo port used by the tool.
type fakeTodoSessions struct {
	sess      session.Session
	getErr    error
	setErr    error
	setID     string
	setCalled bool
}

func (f *fakeTodoSessions) Todos(_ context.Context, _ string) ([]session.Todo, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.sess.Todos, nil
}

// SetTodos is the narrow write the tool actually performs now: only the
// list, so a turn saving usage on the same row cannot carry a stale copy
// of it back over the top.
func (f *fakeTodoSessions) SetTodos(_ context.Context, sessionID string, todos []session.Todo) error {
	f.setCalled = true
	f.setID = sessionID
	if f.setErr != nil {
		return f.setErr
	}
	f.sess.Todos = todos
	return nil
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
func TestTodosTool_InfrastructureErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fake *fakeTodoSessions
		want string
	}{
		{name: "load", fake: &fakeTodoSessions{getErr: errors.New("session unavailable")}, want: "failed to get session"},
		{name: "save", fake: &fakeTodoSessions{sess: session.Session{ID: "session-1"}, setErr: errors.New("session unavailable")}, want: "failed to save todos"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := NewTodosTool(tc.fake)
			input, err := json.Marshal(TodosParams{Todos: []TodoItem{{Content: "test", Status: "pending"}}})
			require.NoError(t, err)
			_, err = tool.Run(context.WithValue(t.Context(), SessionIDContextKey, "session-1"), fantasy.ToolCall{ID: "call-1", Input: string(input)})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

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

// TestTodosTool_NewCycleDropsResentCompletedItems is the regression test for
// a finished todo list reopening: todos.md tells the model never to drop
// completed items, so after finishing everything it resends the whole
// completed list with the new task appended. The stored list must hold only
// the new cycle's work — otherwise the session panel, which hides the
// section once everything is completed, pops it back with the old tail
// still attached.
func TestTodosTool_NewCycleDropsResentCompletedItems(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
			{Content: "ship it", Status: session.TodoStatusCompleted},
		},
	}}

	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "write tests", Status: "completed"},
		{Content: "  SHIP IT  ", Status: "completed"},
		{Content: "monitor rollout", Status: "in_progress", ActiveForm: "Monitoring rollout"},
	})

	require.Equal(t, []session.Todo{{
		Content:    "monitor rollout",
		Status:     session.TodoStatusInProgress,
		ActiveForm: "Monitoring rollout",
	}}, todos, "only the new cycle's work survives, whitespace/case variants included")
	require.True(t, meta.IsNew)
	require.Equal(t, 0, meta.Completed)
	require.Equal(t, 1, meta.Total)
	require.Equal(t, "Monitoring rollout", meta.JustStarted)
	require.Empty(t, meta.JustCompleted)
}

// TestTodosTool_NewCycleKeepsCompletedItemsTheOldListNeverHad covers the
// other side of that filter: a task the model reports completed which the
// finished list never contained is new-cycle work done in the same call, so
// it must stay.
func TestTodosTool_NewCycleKeepsCompletedItemsTheOldListNeverHad(t *testing.T) {
	t.Parallel()

	sessions := &fakeTodoSessions{sess: session.Session{
		ID: "session-1",
		Todos: []session.Todo{
			{Content: "write tests", Status: session.TodoStatusCompleted},
		},
	}}

	todos, meta := runTodosTool(t, sessions, []TodoItem{
		{Content: "write tests", Status: "completed"},
		{Content: "cut release", Status: "completed"},
		{Content: "monitor rollout", Status: "in_progress", ActiveForm: "Monitoring rollout"},
	})

	require.Len(t, todos, 2)
	require.Equal(t, "cut release", todos[0].Content)
	require.Equal(t, "monitor rollout", todos[1].Content)
	require.True(t, meta.IsNew)
	require.Equal(t, 1, meta.Completed)
	require.Equal(t, 2, meta.Total)
}
