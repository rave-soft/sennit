package proto_test

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/stretchr/testify/require"
)

func TestTodoAliasPreservesSessionJSONContract(t *testing.T) {
	t.Parallel()

	var todo proto.Todo
	require.NoError(t, json.Unmarshal([]byte(`{"content":"future work","status":"blocked_by_future_server","active_form":"Waiting"}`), &todo))
	require.Equal(t, session.TodoStatus("blocked_by_future_server"), todo.Status)

	data, err := json.Marshal(todo)
	require.NoError(t, err)
	require.JSONEq(t, `{"content":"future work","status":"blocked_by_future_server","active_form":"Waiting"}`, string(data))
}

func TestSessionTodoAliasPreservesNilAndEmptySlices(t *testing.T) {
	t.Parallel()

	nilSession := proto.Session{}
	emptySession := proto.Session{Todos: []proto.Todo{}}
	require.Nil(t, nilSession.Todos)
	require.NotNil(t, emptySession.Todos)
	require.Len(t, emptySession.Todos, 0)
}
