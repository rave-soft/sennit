package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/session"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

//go:embed todos.md
var todosDescription string

const TodosToolName = "todos"

// normalizeTodoContent folds whitespace and case so the merge logic below
// treats "Run tests" and "  run tests  " as the same item.
func normalizeTodoContent(content string) string {
	return strings.ToLower(strings.TrimSpace(content))
}

type TodosParams struct {
	Todos []TodoItem `json:"todos" description:"The updated todo list"`
}

type TodoItem struct {
	Content    string `json:"content" description:"What needs to be done (imperative form)"`
	Status     string `json:"status" description:"Task status: pending, in_progress, or completed"`
	ActiveForm string `json:"active_form" description:"Present continuous form (e.g., 'Running tests')"`
}

type TodosResponseMetadata struct {
	IsNew         bool           `json:"is_new"`
	Todos         []session.Todo `json:"todos"`
	JustCompleted []string       `json:"just_completed,omitempty"`
	JustStarted   string         `json:"just_started,omitempty"`
	Completed     int            `json:"completed"`
	Total         int            `json:"total"`
}

func NewTodosTool(sessions sessionstore.Service) fantasy.AgentTool {
	return withToolParameterSchema(fantasy.NewAgentTool(
		TodosToolName,
		todosDescription,
		func(ctx context.Context, params TodosParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("managing todos")
			}

			currentSession, err := sessions.Get(ctx, sessionID)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to get session: %w", err)
			}

			startsNewCycle := len(currentSession.Todos) > 0 && !session.HasIncompleteTodos(currentSession.Todos)
			if startsNewCycle {
				startsNewCycle = false
				for _, item := range params.Todos {
					if session.TodoStatus(item.Status) != session.TodoStatusCompleted {
						startsNewCycle = true
						break
					}
				}
			}
			isNew := len(currentSession.Todos) == 0 || startsNewCycle
			oldStatusByContent := make(map[string]session.TodoStatus)
			for _, todo := range currentSession.Todos {
				oldStatusByContent[todo.Content] = todo.Status
			}

			for _, item := range params.Todos {
				switch item.Status {
				case "pending", "in_progress", "completed":
				default:
					// A bad status is bad input the model supplied, not a
					// failure of this tool — it can see the message and
					// resend the call with a valid status, so this is a
					// text response rather than a Go error.
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"invalid status %q for todo %q: must be pending, in_progress, or completed",
						item.Status, item.Content)), nil
				}
			}

			// A model that has just finished every task keeps resending the
			// whole finished list and appends the new work to it — todos.md
			// tells it never to drop completed items, and taken literally
			// that is what it does. Left alone, a list the user already saw
			// close reopens with its old tail attached: the session panel
			// hides the section once everything is completed and pops it
			// back the moment a new in_progress item arrives. So when this
			// call starts a new cycle, drop the carried-over items — the
			// ones the stored list already held, still completed — and keep
			// only what belongs to the new cycle. Items the model reports
			// completed that the old list never had are genuinely new-cycle
			// work and stay.
			if startsNewCycle {
				carried := make(map[string]bool, len(currentSession.Todos))
				for _, old := range currentSession.Todos {
					carried[normalizeTodoContent(old.Content)] = true
				}
				fresh := make([]TodoItem, 0, len(params.Todos))
				for _, item := range params.Todos {
					if session.TodoStatus(item.Status) == session.TodoStatusCompleted &&
						carried[normalizeTodoContent(item.Content)] {
						continue
					}
					fresh = append(fresh, item)
				}
				params.Todos = fresh
			}

			todos := make([]session.Todo, len(params.Todos))
			var justCompleted []string
			var justStarted string
			completedCount := 0

			for i, item := range params.Todos {
				todos[i] = session.Todo{
					Content:    item.Content,
					Status:     session.TodoStatus(item.Status),
					ActiveForm: item.ActiveForm,
				}

				newStatus := session.TodoStatus(item.Status)
				oldStatus, existed := oldStatusByContent[item.Content]

				if newStatus == session.TodoStatusCompleted {
					completedCount++
					if existed && oldStatus != session.TodoStatusCompleted {
						justCompleted = append(justCompleted, item.Content)
					}
				}

				if newStatus == session.TodoStatusInProgress {
					if !existed || oldStatus != session.TodoStatusInProgress {
						if item.ActiveForm != "" {
							justStarted = item.ActiveForm
						} else {
							justStarted = item.Content
						}
					}
				}
			}

			// Small/local models don't reliably repeat every previously
			// completed item on each todos call. Treating params.Todos as a
			// full replacement would silently drop those — merge back any
			// old completed todo the model didn't mention this time, unless
			// params.Todos is empty, which is an explicit reset. Items the
			// model DID mention (any status) are left alone: the incoming
			// copy wins outright.
			if len(params.Todos) > 0 && !startsNewCycle {
				seen := make(map[string]bool, len(todos))
				for _, t := range todos {
					seen[normalizeTodoContent(t.Content)] = true
				}
				appended := make(map[string]bool)
				for _, old := range currentSession.Todos {
					if old.Status != session.TodoStatusCompleted {
						continue
					}
					key := normalizeTodoContent(old.Content)
					if seen[key] || appended[key] {
						continue
					}
					appended[key] = true
					todos = append(todos, old)
					completedCount++
				}
			}

			// A narrow write, not a full-row Save: this tool runs
			// mid-turn, and the turn saves the same row for usage. Each
			// full-row write carried a stale copy of what the other had
			// just written, so a todo list could vanish the moment a step
			// finished.
			currentSession.Todos = todos
			if err := sessions.SetTodos(ctx, currentSession.ID, todos); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to save todos: %w", err)
			}

			response := "Todo list updated successfully.\n\n"

			pendingCount := 0
			inProgressCount := 0

			for _, todo := range todos {
				switch todo.Status {
				case session.TodoStatusPending:
					pendingCount++
				case session.TodoStatusInProgress:
					inProgressCount++
				}
			}

			response += fmt.Sprintf("Status: %d pending, %d in progress, %d completed\n",
				pendingCount, inProgressCount, completedCount)

			response += "Todos have been modified successfully. Ensure that you continue to use the todo list to track your progress. Please proceed with the current tasks if applicable."

			metadata := TodosResponseMetadata{
				IsNew:         isNew,
				Todos:         todos,
				JustCompleted: justCompleted,
				JustStarted:   justStarted,
				Completed:     completedCount,
				Total:         len(todos),
			}

			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(response), metadata), nil
		},
	), map[string]toolParameterSchema{"todos.items.status": {enum: []string{"pending", "in_progress", "completed"}}})
}
