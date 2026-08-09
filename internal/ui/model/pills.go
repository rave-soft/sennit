package model

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/chat"
	"github.com/rave-soft/braid/internal/ui/styles"
)

const (
	// pillHeightWithBorder is the height of a pill including its border.
	pillHeightWithBorder = 3
	// maxTaskDisplayLength is the maximum length of a task name in the pill.
	maxTaskDisplayLength = 40
	// maxQueueDisplayLength is the maximum length of a queue item in the list.
	maxQueueDisplayLength = 60
)

// pillSection represents which section of the pills panel is focused.
type pillSection int

const (
	pillSectionTodos pillSection = iota
	pillSectionQueue
)

// pillsPanelState holds the expand/focus/render state of the pills panel
// (todo progress + queued-prompt pills below the chat).
type pillsPanelState struct {
	expanded       bool
	autoExpanded   bool
	focusedSection pillSection
	view           string
}

// hasIncompleteTodos returns true if there are any non-completed todos.
func hasIncompleteTodos(todos []session.Todo) bool {
	return session.HasIncompleteTodos(todos)
}

// hasInProgressTodo returns true if there is at least one in-progress todo.
func hasInProgressTodo(todos []session.Todo) bool {
	for _, todo := range todos {
		if todo.Status == session.TodoStatusInProgress {
			return true
		}
	}
	return false
}

// queuePill renders the queue count pill with gradient triangles. Pills always
// render with a border; focus within the expanded panel is conveyed by the list
// shown below the pills, not by hiding a pill's border.
func queuePill(queue int, t *styles.Styles) string {
	if queue <= 0 {
		return ""
	}
	triangles := styles.ForegroundGrad(t.Pills.QueueIconBase, "▶▶▶▶▶▶▶▶▶", false, t.Pills.QueueGradFromColor, t.Pills.QueueGradToColor)
	if queue < len(triangles) {
		triangles = triangles[:queue]
	}

	text := t.Pills.QueueLabel.Render(fmt.Sprintf("%d Queued", queue))
	content := fmt.Sprintf("%s %s", strings.Join(triangles, ""), text)
	return t.Pills.Focused.Render(content)
}

// todoPill renders the todo progress pill with optional spinner and task name.
func todoPill(todos []session.Todo, spinnerView string, panelFocused bool, t *styles.Styles) string {
	if !hasIncompleteTodos(todos) {
		return ""
	}

	completed := 0
	var currentTodo *session.Todo
	for i := range todos {
		switch todos[i].Status {
		case session.TodoStatusCompleted:
			completed++
		case session.TodoStatusInProgress:
			if currentTodo == nil {
				currentTodo = &todos[i]
			}
		}
	}

	total := len(todos)

	label := t.Pills.TodoLabel.Render("To-Do")
	progress := t.Pills.TodoProgress.Render(fmt.Sprintf("%d/%d", completed, total))

	var content string
	if panelFocused {
		content = fmt.Sprintf("%s %s", label, progress)
	} else if currentTodo != nil {
		taskText := currentTodo.Content
		if currentTodo.ActiveForm != "" {
			taskText = currentTodo.ActiveForm
		}
		if ansi.StringWidth(taskText) > maxTaskDisplayLength {
			taskText = ansi.Truncate(taskText, maxTaskDisplayLength-1, "…")
		}
		task := t.Pills.TodoCurrentTask.Render(taskText)
		content = fmt.Sprintf("%s %s %s  %s", spinnerView, label, progress, task)
	} else {
		content = fmt.Sprintf("%s %s", label, progress)
	}

	return t.Pills.Focused.Render(content)
}

// todoList renders the expanded todo list.
func todoList(sessionTodos []session.Todo, spinnerView string, t *styles.Styles, width int) string {
	return chat.FormatTodosList(t, sessionTodos, spinnerView, width)
}

// queueList renders the expanded queue items list.
func queueList(queueItems []string, t *styles.Styles) string {
	if len(queueItems) == 0 {
		return ""
	}

	var lines []string
	for _, item := range queueItems {
		text := item
		if ansi.StringWidth(text) > maxQueueDisplayLength {
			text = ansi.Truncate(text, maxQueueDisplayLength-1, "…")
		}
		prefix := t.Pills.QueueItemPrefix.Render() + " "
		lines = append(lines, prefix+t.Pills.QueueItemText.Render(text))
	}

	return strings.Join(lines, "\n")
}

// pillsHeightReasonableTerminalHeight is the minimum terminal height at which
// we auto-expand pills when there are incomplete todos.
const pillsHeightReasonableTerminalHeight = 40

// autoExpandPillsIfReasonable expands the pills panel if the terminal has
// enough vertical space to show the expanded list comfortably.
func (m *UI) autoExpandPillsIfReasonable() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	if m.activeInline != nil {
		return nil
	}
	if m.height < pillsHeightReasonableTerminalHeight {
		return nil
	}
	hasPills := hasIncompleteTodos(m.session.Todos) || m.wsCache.promptQueue > 0
	if !hasPills {
		return nil
	}
	if m.pills.expanded {
		return nil
	}
	if m.pills.autoExpanded {
		return nil
	}
	m.pills.expanded = true
	m.pills.autoExpanded = true
	if hasIncompleteTodos(m.session.Todos) {
		m.pills.focusedSection = pillSectionTodos
	} else {
		m.pills.focusedSection = pillSectionQueue
	}
	m.updateLayoutAndSize()
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}
	return nil
}

// togglePillsExpanded toggles the pills panel expansion state.
func (m *UI) togglePillsExpanded() tea.Cmd {
	if !m.hasSession() {
		return nil
	}
	hasPills := hasIncompleteTodos(m.session.Todos) || m.wsCache.promptQueue > 0
	if !hasPills {
		return nil
	}
	m.pills.expanded = !m.pills.expanded
	if m.pills.expanded {
		if hasIncompleteTodos(m.session.Todos) {
			m.pills.focusedSection = pillSectionTodos
		} else {
			m.pills.focusedSection = pillSectionQueue
		}
	}
	m.updateLayoutAndSize()

	// Make sure to follow scroll if follow is enabled when toggling pills.
	// Note: uses ScrollToBottom (no scrollbar) since this is layout adjustment,
	// not user-initiated scrolling.
	if m.chat.Follow() {
		m.chat.ScrollToBottom()
	}

	return nil
}

// switchPillSection changes focus between todo and queue sections.
func (m *UI) switchPillSection(dir int) tea.Cmd {
	if !m.pills.expanded || !m.hasSession() {
		return nil
	}
	hasIncompleteTodos := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0

	if dir < 0 && m.pills.focusedSection == pillSectionQueue && hasIncompleteTodos {
		m.pills.focusedSection = pillSectionTodos
		m.updateLayoutAndSize()
		return nil
	}
	if dir > 0 && m.pills.focusedSection == pillSectionTodos && hasQueue {
		m.pills.focusedSection = pillSectionQueue
		m.updateLayoutAndSize()
		return nil
	}
	return nil
}

// effectiveFocusedSection returns the pill section that should be treated as
// focused for rendering. The stored focusedPillSection can go stale when its
// section loses all content (for example todos complete while the panel is open,
// or it defaults to todos before any todos exist). In that case we fall through
// to whichever section still has content so the expanded list stays populated.
func (m *UI) effectiveFocusedSection() pillSection {
	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0
	switch m.pills.focusedSection {
	case pillSectionQueue:
		if hasQueue {
			return pillSectionQueue
		}
		if hasIncomplete {
			return pillSectionTodos
		}
	default: // pillSectionTodos
		if hasIncomplete {
			return pillSectionTodos
		}
		if hasQueue {
			return pillSectionQueue
		}
	}
	return m.pills.focusedSection
}

// pillsAreaHeight calculates the total height needed for the pills area.
func (m *UI) pillsAreaHeight() int {
	if !m.hasSession() {
		return 0
	}
	// Suppress pills when an inline editor (e.g. question form) is active
	// to avoid competing for screen space.
	if m.activeInline != nil {
		return 0
	}
	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0
	hasPills := hasIncomplete || hasQueue
	if !hasPills {
		return 0
	}

	pillsAreaHeight := pillHeightWithBorder
	if m.pills.expanded {
		switch m.effectiveFocusedSection() {
		case pillSectionTodos:
			if hasIncomplete {
				pillsAreaHeight += len(m.session.Todos)
			}
		case pillSectionQueue:
			if hasQueue {
				pillsAreaHeight += m.wsCache.promptQueue
			}
		}
	}
	return pillsAreaHeight
}

// renderPills renders the pills panel and stores it in m.pills.view.
func (m *UI) renderPills() {
	m.pills.view = ""
	if !m.hasSession() {
		return
	}
	// Suppress pills when an inline editor (e.g. question form) is active.
	if m.activeInline != nil {
		return
	}

	width := m.layout.pills.Dx()
	if width <= 0 {
		return
	}

	paddingLeft := 3
	contentWidth := max(width-paddingLeft, 0)

	hasIncomplete := hasIncompleteTodos(m.session.Todos)
	hasQueue := m.wsCache.promptQueue > 0

	if !hasIncomplete && !hasQueue {
		return
	}

	t := m.com.Styles
	effective := m.effectiveFocusedSection()
	todosFocused := m.pills.expanded && effective == pillSectionTodos
	queueFocused := m.pills.expanded && effective == pillSectionQueue

	inProgressIcon := t.Tool.TodoInProgressIcon.Render(styles.SpinnerIcon)
	if m.todoIsSpinning {
		inProgressIcon = m.todoSpinner.View()
	}

	var pills []string
	if hasIncomplete {
		pills = append(pills, todoPill(m.session.Todos, inProgressIcon, m.pills.expanded, t))
	}
	if hasQueue {
		pills = append(pills, queuePill(m.wsCache.promptQueue, t))
	}

	var expandedList string
	if m.pills.expanded {
		if todosFocused && hasIncomplete {
			expandedList = todoList(m.session.Todos, inProgressIcon, t, contentWidth)
		} else if queueFocused && hasQueue {
			// Render from the memoized queue (fetched off-thread, see
			// workspace_cache.go): renderPills runs on the Update/View
			// path and must never block on a workspace round-trip.
			if len(m.wsCache.promptQueueItems) > 0 {
				expandedList = queueList(m.wsCache.promptQueueItems, t)
			}
		}
	}

	if len(pills) == 0 {
		return
	}

	pillsRow := lipgloss.JoinHorizontal(lipgloss.Top, pills...)

	helpDesc := "open"
	if m.pills.expanded {
		helpDesc = "close"
	}
	helpKey := t.Pills.HelpKey.Render("ctrl+t")
	helpText := t.Pills.HelpText.Render(helpDesc)
	helpHint := lipgloss.JoinHorizontal(lipgloss.Center, helpKey, " ", helpText)
	pillsRow = lipgloss.JoinHorizontal(lipgloss.Center, pillsRow, " ", helpHint)

	pillsArea := pillsRow
	if expandedList != "" {
		pillsArea = lipgloss.JoinVertical(lipgloss.Left, pillsRow, expandedList)
	}

	m.pills.view = t.Pills.Area.MaxWidth(width).PaddingLeft(paddingLeft).Render(pillsArea)
}
