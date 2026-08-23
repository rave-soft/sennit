// Package presentation provides shared, pure UI formatting and row-rendering
// helpers. It deliberately depends only on leaf packages (styles, and
// session for its Todo type) so chat and model can share presentation rules
// without an import cycle.
package presentation

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/styles"
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

// IsLikelyPath reports whether s looks unambiguously like a filesystem
// path, and so should be truncated from the left rather than the right.
// Absolute paths qualify, as do relative ones containing a separator;
// anything carrying shell-ish characters or whitespace does not, since a
// command's meaning lives in its head.
//
// "\" is deliberately not in that shell-ish set: on Windows it is the path
// separator, not an escape character, so excluding it made every Windows
// path (e.g. "C:\Users\x\file.go") fail this check. That sent it through
// plain right-truncation instead of TruncatePath, cutting the string
// somewhere mid-path and dropping the file name — the one part of a path
// that actually identifies it. Any escaped-space case is still excluded by
// the space in this set regardless.
func IsLikelyPath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n¶'\"|&;<>$`*?(){}[]") {
		return false
	}
	if filepath.IsAbs(s) {
		return true
	}
	return strings.ContainsAny(s, "/\\")
}

// TruncatePath fits a filesystem path into width, eliding the head rather
// than the tail: the file name is what identifies the file, so
// "…/chat/tools_render.go" is useful where "internal/ui/chat/tools_re…"
// is not. When even the file name alone does not fit it is cut on the
// right as a last resort — half a name still beats a bare ellipsis.
//
// Callers that may hold something other than a path should gate on
// IsLikelyPath first; this function assumes its input is one.
func TruncatePath(path string, width int) string {
	if width < 0 || ansi.StringWidth(path) <= width {
		return path
	}
	base := filepath.Base(path)
	// One cell goes to the leading "…"; if the name cannot fit beside it,
	// there is nothing left to preserve.
	if ansi.StringWidth(base)+1 > width {
		return ansi.Truncate(base, width, "…")
	}
	// ansi.TruncateLeft removes n graphemes from the start; pick n so the
	// result plus the "…" prefix fits exactly in width.
	n := ansi.StringWidth(path) - width + 1
	return ansi.TruncateLeft(path, n, "…")
}

// TruncatePathAware truncates s from the left when it is a path and from
// the right otherwise — the rule most one-line UI rows want when a value
// may be either.
func TruncatePathAware(s string, width int) string {
	if width < 0 || ansi.StringWidth(s) <= width {
		return s
	}
	if IsLikelyPath(s) {
		return TruncatePath(s, width)
	}
	return ansi.Truncate(s, width, "…")
}

// Truncate shortens s to width cells, marking the cut with an ellipsis. It
// is the plain (non-path-aware) counterpart to TruncatePathAware, for
// table cells and labels where the head, not the tail, carries the
// meaning — the ANSI-safe replacement for ad hoc rune-slicing.
func Truncate(s string, width int) string {
	if width < 0 || ansi.StringWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// PadTo truncates (via Truncate) or right-pads s with spaces so it occupies
// exactly n cells — the shape a fixed-width table column needs. n <= 0
// yields an empty string.
func PadTo(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = Truncate(s, n)
	if pad := n - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}
