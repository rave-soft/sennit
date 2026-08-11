package chat

import (
	"encoding/json"
	"testing"

	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestTodosRenderTool_UpdateWithNoDeltaStillShowsBody is the regression test
// for the empty-transcript-body bug: per commit b1efdc60, todos is a
// deliberate exception to the one-line tool-row redesign — it always shows
// the full current list, not a delta summary, in the chat transcript. The
// old body-selection logic only populated body when everything was just
// completed (allCompleted) or something just started (meta.JustStarted !=
// ""); an update with neither — e.g. the model only adding new pending
// items, or a reorder — fell through to an empty body and rendered as a
// bare header with no list at all.
func TestTodosRenderTool_UpdateWithNoDeltaStillShowsBody(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()

	todos := []session.Todo{
		{Content: "write the plan", Status: session.TodoStatusCompleted},
		{Content: "implement the fix", Status: session.TodoStatusInProgress, ActiveForm: "Implementing the fix"},
		{Content: "add a test", Status: session.TodoStatusPending},
		{Content: "polish docs", Status: session.TodoStatusPending},
	}
	meta := tools.TodosResponseMetadata{
		IsNew: false,
		Todos: todos,
		// Deliberately empty: nothing "just" completed or started this
		// round from the delta's point of view.
		JustCompleted: nil,
		JustStarted:   "",
		Completed:     1,
		Total:         4,
	}
	metaJSON, err := json.Marshal(meta)
	require.NoError(t, err)

	params := tools.TodosParams{Todos: []tools.TodoItem{
		{Content: "write the plan", Status: "completed"},
		{Content: "implement the fix", Status: "in_progress", ActiveForm: "Implementing the fix"},
		{Content: "add a test", Status: "pending"},
		{Content: "polish docs", Status: "pending"},
	}}
	inputJSON, err := json.Marshal(params)
	require.NoError(t, err)

	toolCall := message.ToolCall{
		ID:       "tc-todos",
		Name:     tools.TodosToolName,
		Input:    string(inputJSON),
		Finished: true,
	}
	result := &message.ToolResult{
		ToolCallID: toolCall.ID,
		Metadata:   string(metaJSON),
	}

	ctx := &TodosToolRenderContext{}
	out := ctx.RenderTool(&sty, 80, &ToolRenderOpts{
		ToolCall: toolCall,
		Result:   result,
		Status:   ToolStatusSuccess,
	})

	require.NotEmpty(t, out, "rendered todos tool call must not be a bare header")
	for _, todo := range todos {
		text := todo.Content
		if todo.Status == session.TodoStatusInProgress && todo.ActiveForm != "" {
			text = todo.ActiveForm
		}
		require.Containsf(t, out, text, "rendered output must contain every todo, including %q", text)
	}
}
