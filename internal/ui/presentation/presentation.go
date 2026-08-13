// Package presentation provides shared, pure UI formatting and row-rendering
// helpers. It deliberately depends only on leaf UI packages so chat and model
// can share presentation rules without an import cycle.
package presentation

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// FormatElapsed renders a duration for a compact status line.
func FormatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// FormatTokenCount renders large token counts compactly.
func FormatTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// JoinStatusParts joins already ordered status parts and truncates the whole
// line. Empty parts are omitted, so optional segments retain each caller's
// fallback behavior without producing stray separators.
func JoinStatusParts(parts []string, width int) string {
	filtered := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			filtered = append(filtered, part)
		}
	}
	line := strings.Join(filtered, " · ")
	if width < 0 {
		return line
	}
	return ansi.Truncate(line, width, "…")
}

// TodoBuckets groups todos by presentation status. Every bucket preserves the
// source order, and statuses unknown to this version are placed in Pending.
type TodoBuckets struct {
	InProgress []session.Todo
	Pending    []session.Todo
	Completed  []session.Todo
}

// BucketTodos groups todos into stable status buckets. Unknown statuses are
// deliberately treated as pending so they remain visible and actionable.
func BucketTodos(todos []session.Todo) TodoBuckets {
	buckets := TodoBuckets{
		InProgress: make([]session.Todo, 0, len(todos)),
		Pending:    make([]session.Todo, 0, len(todos)),
		Completed:  make([]session.Todo, 0, len(todos)),
	}
	for _, todo := range todos {
		switch todo.Status {
		case session.TodoStatusInProgress:
			buckets.InProgress = append(buckets.InProgress, todo)
		case session.TodoStatusCompleted:
			buckets.Completed = append(buckets.Completed, todo)
		default:
			buckets.Pending = append(buckets.Pending, todo)
		}
	}
	return buckets
}

// TodoRowOptions controls context-specific visual distinctions while sharing
// status classification, icons, ActiveForm fallback, and ANSI-safe truncation.
type TodoRowOptions struct {
	InProgressIcon         string
	CompletedMuted         bool
	CompletedStrikethrough bool
}

// RenderTodoRow renders one todo row. ActiveForm is used only by in-progress
// todos and falls back to Content; unknown statuses use the pending treatment.
func RenderTodoRow(todo session.Todo, sty *styles.Styles, width int, opts TodoRowOptions) string {
	prefix := ""
	textStyle := sty.Tool.TodoItem
	switch todo.Status {
	case session.TodoStatusCompleted:
		prefix = sty.Tool.TodoCompletedIcon.Render(styles.TodoCompletedIcon) + " "
		if opts.CompletedMuted {
			textStyle = sty.Pills.TodoCurrentTask
		}
		if opts.CompletedStrikethrough {
			textStyle = textStyle.Strikethrough(true)
		}
	case session.TodoStatusInProgress:
		prefix = sty.Tool.TodoInProgressIcon.Render(opts.InProgressIcon + " ")
	default:
		prefix = sty.Tool.TodoPendingIcon.Render(styles.TodoPendingIcon) + " "
	}
	text := todo.Content
	if todo.Status == session.TodoStatusInProgress && todo.ActiveForm != "" {
		text = todo.ActiveForm
	}
	return ansi.Truncate(prefix+textStyle.Render(text), width, "…")
}
