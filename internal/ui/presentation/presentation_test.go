package presentation

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/styles"
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
	sty := styles.SennitDark()
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

// TestTruncatePath_KeepsFileName pins the rule every one-line path row
// follows: the head is what gets elided, so the file name — the part that
// identifies the file — always survives.
func TestTruncatePath_KeepsFileName(t *testing.T) {
	t.Parallel()

	path := "internal/ui/chat/very/deeply/nested/tools_render.go"
	out := TruncatePath(path, 24)
	require.LessOrEqual(t, ansi.StringWidth(out), 24)
	require.Contains(t, out, "tools_render.go")
	require.True(t, strings.HasPrefix(out, "…"), "got %q", out)
}

// TestTruncatePath_FitsUnchanged proves a path that already fits gets no
// gratuitous ellipsis.
func TestTruncatePath_FitsUnchanged(t *testing.T) {
	t.Parallel()

	path := "internal/ui/chat/tools_render.go"
	require.Equal(t, path, TruncatePath(path, 80))
}

// TestTruncatePath_FileNameAloneTooLong covers the last resort: with no
// room even for the name, it is cut on the right rather than leaving a
// bare ellipsis.
func TestTruncatePath_FileNameAloneTooLong(t *testing.T) {
	t.Parallel()

	out := TruncatePath("internal/ui/a-very-long-file-name.go", 10)
	require.LessOrEqual(t, ansi.StringWidth(out), 10)
	require.True(t, strings.HasPrefix(out, "a-very"), "got %q", out)
}

// TestTruncatePathAware_NonPathTruncatesRight proves the left-truncation
// rule is scoped to paths: a command carries its meaning in the head.
func TestTruncatePathAware_NonPathTruncatesRight(t *testing.T) {
	t.Parallel()

	out := TruncatePathAware("go test ./internal/ui/... -timeout 600s", 20)
	require.LessOrEqual(t, ansi.StringWidth(out), 20)
	require.True(t, strings.HasPrefix(out, "go test"), "got %q", out)
	require.True(t, strings.HasSuffix(out, "…"), "got %q", out)
}

// TestIsLikelyPath covers the gate: absolute and separator-bearing
// relative paths qualify; a bare word or anything shell-ish does not.
func TestIsLikelyPath(t *testing.T) {
	t.Parallel()

	require.True(t, IsLikelyPath("/etc/hosts"))
	require.True(t, IsLikelyPath("internal/ui/chat/file.go"))
	require.False(t, IsLikelyPath("file.go"), "no separator, nothing to elide")
	require.False(t, IsLikelyPath("ls -la /tmp"))
	require.False(t, IsLikelyPath("cat a.txt | grep x"))
	require.False(t, IsLikelyPath(""))
}

// TestTruncate covers the plain (non-path-aware) ellipsis truncation used
// by table cells: unchanged when it already fits, cut with an ellipsis
// otherwise, and never widened by a negative width.
func TestTruncate(t *testing.T) {
	t.Parallel()

	require.Equal(t, "hello", Truncate("hello", 80))
	require.Equal(t, "hello", Truncate("hello", -1))
	out := Truncate("hello world", 8)
	require.LessOrEqual(t, ansi.StringWidth(out), 8)
	require.True(t, strings.HasSuffix(out, "…"), "got %q", out)
}

// TestPadTo covers the table-column helper: padding a short value out to
// width, truncating (with ellipsis) a value that overruns it, and the
// n<=0 edge case a dropped column relies on to render as nothing.
func TestPadTo(t *testing.T) {
	t.Parallel()

	require.Equal(t, "hi   ", PadTo("hi", 5))
	require.Equal(t, "hi", PadTo("hi", 2), "exact fit needs no padding")
	out := PadTo("a very long value", 8)
	require.Equal(t, 8, ansi.StringWidth(out))
	require.True(t, strings.HasSuffix(out, "…"), "got %q", out)
	require.Equal(t, "", PadTo("anything", 0))
}
