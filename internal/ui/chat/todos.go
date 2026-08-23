package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/ui/presentation"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Todos Tool
// -----------------------------------------------------------------------------

// TodosToolRenderContext renders todos tool messages.
type TodosToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (t *TodosToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "To-Do", opts)
	}

	var params tools.TodosParams
	var meta tools.TodosResponseMetadata
	var headerText string
	var body string

	// Parse params for pending state (before result is available).
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err == nil {
		completedCount := 0
		inProgressTask := ""
		for _, todo := range params.Todos {
			if todo.Status == "completed" {
				completedCount++
			}
			if todo.Status == "in_progress" {
				if todo.ActiveForm != "" {
					inProgressTask = todo.ActiveForm
				} else {
					inProgressTask = todo.Content
				}
			}
		}

		// Default display from params (used when pending or no metadata).
		ratio := sty.Tool.TodoRatio.Render(fmt.Sprintf("%d/%d", completedCount, len(params.Todos)))
		headerText = ratio
		if inProgressTask != "" {
			headerText = fmt.Sprintf("%s · %s", ratio, inProgressTask)
		}

		// If we have metadata, use it for richer display.
		if opts.HasResult() && opts.Result.Metadata != "" {
			if err := json.Unmarshal([]byte(opts.Result.Metadata), &meta); err == nil {
				if meta.IsNew {
					if meta.JustStarted != "" {
						headerText = fmt.Sprintf("created %d todos, starting first", meta.Total)
					} else {
						headerText = fmt.Sprintf("created %d todos", meta.Total)
					}
					body = FormatTodosList(sty, meta.Todos, styles.ArrowRightIcon, width)
				} else {
					// Build header based on what changed.
					hasCompleted := len(meta.JustCompleted) > 0
					hasStarted := meta.JustStarted != ""
					allCompleted := meta.Completed == meta.Total

					ratio := sty.Tool.TodoRatio.Render(fmt.Sprintf("%d/%d", meta.Completed, meta.Total))
					if hasCompleted && hasStarted {
						text := sty.Tool.TodoStatusNote.Render(fmt.Sprintf(" · completed %d, starting next", len(meta.JustCompleted)))
						headerText = fmt.Sprintf("%s%s", ratio, text)
					} else if hasCompleted {
						text := sty.Tool.TodoStatusNote.Render(fmt.Sprintf(" · completed %d", len(meta.JustCompleted)))
						if allCompleted {
							text = sty.Tool.TodoStatusNote.Render(" · completed all")
						}
						headerText = fmt.Sprintf("%s%s", ratio, text)
					} else if hasStarted {
						headerText = fmt.Sprintf("%s%s", ratio, sty.Tool.TodoStatusNote.Render(" · starting task"))
					} else {
						headerText = ratio
					}

					// Todos is a deliberate exception to the one-line tool-row
					// redesign (see commit b1efdc60): it always shows the
					// full current list body, not just a delta summary. A
					// conditional body here (only on newly-completed or
					// newly-started items) left updates with neither —
					// e.g. the model just adding pending items, or a
					// reorder — rendering as a bare header with no list.
					body = FormatTodosList(sty, meta.Todos, styles.ArrowRightIcon, width)
				}
			}
		}
	}

	toolParams := []string{headerText}
	header := toolHeader(sty, opts.Status, "To-Do", width, opts, toolParams...)
	if opts.Compact {
		return header
	}

	if earlyState, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, earlyState)
	}

	if body == "" {
		return header
	}

	return joinToolParts(header, sty.Tool.Body.Render(body))
}

// FormatTodosList formats a list of todos for display.
func FormatTodosList(sty *styles.Styles, todos []session.Todo, inProgressIcon string, width int) string {
	if len(todos) == 0 {
		return ""
	}

	buckets := presentation.BucketTodos(todos)
	ordered := make([]session.Todo, 0, len(todos))
	ordered = append(ordered, buckets.Completed...)
	ordered = append(ordered, buckets.InProgress...)
	ordered = append(ordered, buckets.Pending...)
	lines := make([]string, 0, len(ordered))
	for _, todo := range ordered {
		lines = append(lines, presentation.RenderTodoRow(todo, sty, width, presentation.TodoRowOptions{
			InProgressIcon: inProgressIcon,
		}))
	}
	return strings.Join(lines, "\n")
}
