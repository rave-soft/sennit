package presentation

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestFormatElapsedAndTokenCount(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		duration time.Duration
		want     string
	}{
		{45 * time.Second, "45s"},
		{4*time.Minute + 12*time.Second, "4m12s"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1h02m"},
	} {
		require.Equal(t, test.want, FormatElapsed(test.duration))
	}
	require.Equal(t, "999", FormatTokenCount(999))
	require.Equal(t, "1.0k", FormatTokenCount(1000))
}

func TestBucketTodosPreservesStabilityAndUnknownFallback(t *testing.T) {
	t.Parallel()
	todos := []session.Todo{
		{Content: "done first", Status: session.TodoStatusCompleted},
		{Content: "unknown first", Status: "future"},
		{Content: "active first", Status: session.TodoStatusInProgress},
		{Content: "pending", Status: session.TodoStatusPending},
		{Content: "done second", Status: session.TodoStatusCompleted},
		{Content: "active second", Status: session.TodoStatusInProgress},
		{Content: "unknown second", Status: "later"},
	}
	names := func(todos []session.Todo) []string {
		out := make([]string, len(todos))
		for i, todo := range todos {
			out[i] = todo.Content
		}
		return out
	}

	buckets := BucketTodos(todos)
	require.Equal(t, []string{"active first", "active second"}, names(buckets.InProgress))
	require.Equal(t, []string{"unknown first", "pending", "unknown second"}, names(buckets.Pending))
	require.Equal(t, []string{"done first", "done second"}, names(buckets.Completed))
}

func TestRenderTodoRowContextOptions(t *testing.T) {
	t.Parallel()
	sty := styles.BraidDark()
	active := RenderTodoRow(session.Todo{Content: "content", ActiveForm: "active form", Status: session.TodoStatusInProgress}, &sty, 80, TodoRowOptions{InProgressIcon: "→"})
	require.Contains(t, ansi.Strip(active), "active form")
	completed := RenderTodoRow(session.Todo{Content: "done", Status: session.TodoStatusCompleted}, &sty, 80, TodoRowOptions{CompletedMuted: true, CompletedStrikethrough: true})
	require.NotEqual(t, ansi.Strip(completed), completed, "completed panel rows retain their ANSI styling")
	unknown := RenderTodoRow(session.Todo{Content: "later", Status: "future"}, &sty, 80, TodoRowOptions{})
	require.Contains(t, ansi.Strip(unknown), styles.TodoPendingIcon)
}

func TestJoinStatusPartsFiltersAndTruncates(t *testing.T) {
	t.Parallel()
	require.Equal(t, "one · two", JoinStatusParts([]string{"one", "", "two"}, 80))
	require.Equal(t, "one…", JoinStatusParts([]string{"one", "two"}, 4))
}
