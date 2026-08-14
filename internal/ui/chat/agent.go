package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/presentation"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// -----------------------------------------------------------------------------
// Agent Tool
// -----------------------------------------------------------------------------

// NestedToolContainer is an interface for tool items that can contain nested tool calls.
type NestedToolContainer interface {
	NestedTools() []ToolMessageItem
	SetNestedTools(tools []ToolMessageItem)
	AddNestedTool(tool ToolMessageItem)
}

// ChildSessionTokenTracker lets the live-update path
// (handleChildSessionUpdate in internal/ui/model/ui.go) push a running
// child-session token count onto a delegation's status line, without this
// package depending on session.Session. Implemented by
// [AgentToolMessageItem] and [AgenticFetchToolMessageItem].
type ChildSessionTokenTracker interface {
	SetChildSessionTokens(prompt, completion int64)
}

// ChildSessionTodoTracker mirrors [ChildSessionTokenTracker] for a running
// delegation's todo list: it lets handleChildSessionUpdate push the child
// session's current todos onto the block without this package depending on
// session.Session's storage details. Implemented by [AgentToolMessageItem]
// and [AgenticFetchToolMessageItem].
type ChildSessionTodoTracker interface {
	SetChildSessionTodos(todos []session.Todo)
}

// AgentToolMessageItem is a message item that represents an agent tool call.
type AgentToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem

	// displayName is the block's title: the built-in agent tool always
	// dispatches to config.AgentTask, so it renders as "task"; a
	// user-defined agent tool's name is already the delegation's identity
	// (toolCall.Name == its cfg.Agents key), so it renders as-is (e.g.
	// "developer"). See agentDisplayName.
	displayName string
	// model and effort are this delegation's configured overrides (empty
	// when the agent inherits the app's defaults — see config.Agent.Model
	// / ReasoningEffort), rendered as a subtitle by renderAgentSubtitle.
	model, effort string

	// startTime and the token counters back the running status line (see
	// renderAgentStatusLine): a long delegation used to render as a bare
	// spinner with no feedback for as long as it took the sub-agent to
	// finish its first tool call — indistinguishable from a hang. Elapsed
	// time is wall-clock local to this item, so it keeps advancing even
	// in client/server mode if child-session events are ever delayed or
	// dropped; the other fields degrade gracefully to "unknown" instead.
	startTime        time.Time
	promptTokens     int64
	completionTokens int64

	// todos mirrors the child session's current todo list (see
	// ChildSessionTodoTracker) — rendered under a still-running
	// delegation only; a finished delegation collapses to a summary and
	// never shows todos (see ToggleExpanded).
	todos []session.Todo

	// duration is frozen the first time SetResult observes a non-nil
	// result (see SetResult below) — i.e. only for a delegation that
	// finishes while this item is live in the UI. An item reconstructed
	// from history gets its result at construction time instead, so
	// duration stays zero ("unknown") rather than reporting the time
	// since page load as if it were the delegation's real runtime.
	duration time.Duration
}

var (
	_ ToolMessageItem          = (*AgentToolMessageItem)(nil)
	_ NestedToolContainer      = (*AgentToolMessageItem)(nil)
	_ ChildSessionTokenTracker = (*AgentToolMessageItem)(nil)
	_ ChildSessionTodoTracker  = (*AgentToolMessageItem)(nil)
)

// NewAgentToolMessageItem creates a new [AgentToolMessageItem]. cfg resolves
// the delegation's display name and any per-agent model/effort override
// (see agentDisplayName); it may be nil, in which case the block falls back
// to the built-in "task" name with no model/effort subtitle.
func NewAgentToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
	cfg *config.Config,
) *AgentToolMessageItem {
	t := &AgentToolMessageItem{startTime: time.Now(), displayName: agentDisplayName(toolCall.Name)}
	if cfg != nil {
		if a, ok := cfg.Agents[t.displayName]; ok {
			t.model = a.Model
			t.effort = a.ReasoningEffort
		}
	}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgentToolRenderContext{agent: t}, canceled)
	// For the agent tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// agentDisplayName resolves a delegation block's title from its tool name.
// The built-in agent tool (tools.AgentToolName) always dispatches to the
// fixed config.AgentTask sub-agent — AgentParams carries no field
// identifying a specific target — so it always renders as "task". A
// user-defined agent tool's own name already is its identity: custom_agent_tool.go
// registers one tool per cfg.Agents entry, named after the map key, so
// toolCall.Name is already the right display name (e.g. "developer").
func agentDisplayName(toolName string) string {
	if toolName == tools.AgentToolName {
		return config.AgentTask
	}
	return toolName
}

// AlwaysSpaced implements list.AlwaysSpaced. A delegation is a visually
// significant boundary — even while it's rendering as a single running
// "Agent ..." status line, it keeps the list's normal gap instead of
// blending into a dense run of one-line tool calls (see
// list.List.gapAt).
func (a *AgentToolMessageItem) AlwaysSpaced() bool {
	return true
}

// DelegationInfoProvider is implemented by tool items that represent a
// delegation into a sub-agent's own session ([AgentToolMessageItem],
// [AgenticFetchToolMessageItem]). It exposes the same identity/model/effort/
// timing data as the collapsed delegation block's subtitle and status line
// (see renderAgentSubtitle, renderDelegationOutcomeLine), for the
// child-session panel in internal/ui/model that replaces the editor while
// the delegation's child session is being viewed.
type DelegationInfoProvider interface {
	// DelegationInfo returns the delegation's display name (e.g. "task",
	// "fetch", or a custom agent's id), its resolved model/effort override
	// (both "" when the delegation has none — agentic_fetch, or an agent
	// tool using the app's default model), when it started, and how long
	// it ran. duration is zero while still running (or, for an item
	// reconstructed from history, if the runtime is genuinely unknown —
	// see AgentToolMessageItem's duration field doc); callers wanting a
	// running delegation's live elapsed time should compute
	// time.Since(startTime) themselves rather than trust duration.
	DelegationInfo() (displayName, model, effort string, startTime time.Time, duration time.Duration)
}

var _ DelegationInfoProvider = (*AgentToolMessageItem)(nil)

// DelegationInfo implements [DelegationInfoProvider].
func (a *AgentToolMessageItem) DelegationInfo() (displayName, model, effort string, startTime time.Time, duration time.Duration) {
	return a.displayName, a.model, a.effort, a.startTime, a.duration
}

// PanelLiveActivityProvider is implemented by running-delegation tool items
// ([AgentToolMessageItem], [AgenticFetchToolMessageItem]) to expose a
// ready-to-draw status line for the session panel's delegation block (see
// internal/ui/model/session_panel.go). It's the exact same styled text
// renderAgentStatusLine already produces for the (now-retired) inline
// pending render — elapsed time, step count, last tool call, token count —
// reused here so the panel never needs its own copy of that formatting, and
// needs no extra IO: everything is already pushed into the item live via
// ChildSessionTokenTracker/AddNestedTool.
type PanelLiveActivityProvider interface {
	PanelStatusLine(sty *styles.Styles, width int) string
}

var _ PanelLiveActivityProvider = (*AgentToolMessageItem)(nil)

// PanelStatusLine implements [PanelLiveActivityProvider].
func (a *AgentToolMessageItem) PanelStatusLine(sty *styles.Styles, width int) string {
	return renderPanelStatusLine(sty, width, a.startTime, a.nestedTools, a.todos, a.promptTokens, a.completionTokens)
}

// SetChildSessionTokens implements [ChildSessionTokenTracker].
func (a *AgentToolMessageItem) SetChildSessionTokens(prompt, completion int64) {
	if a.promptTokens == prompt && a.completionTokens == completion {
		return
	}
	a.promptTokens = prompt
	a.completionTokens = completion
	a.clearCache()
	a.Bump()
}

// SetChildSessionTodos implements [ChildSessionTodoTracker]. Dedupes like
// SetChildSessionTokens: the live-update path re-delivers the full todo
// list on every session save, not just on real changes.
func (a *AgentToolMessageItem) SetChildSessionTodos(todos []session.Todo) {
	if slices.Equal(a.todos, todos) {
		return
	}
	a.todos = todos
	a.clearCache()
	a.Bump()
}

// SetResult freezes the delegation's duration (see the duration field doc)
// the first time a result arrives, then delegates to the embedded setter.
func (a *AgentToolMessageItem) SetResult(res *message.ToolResult) {
	if res != nil && a.duration == 0 {
		a.duration = time.Since(a.startTime)
	}
	a.baseToolMessageItem.SetResult(res)
}

// SetStatus freezes duration on cancellation too — a canceled delegation
// never gets a SetResult call. See the duration field doc.
func (a *AgentToolMessageItem) SetStatus(status ToolStatus) {
	if status == ToolStatusCanceled && a.duration == 0 {
		a.duration = time.Since(a.startTime)
	}
	a.baseToolMessageItem.SetStatus(status)
}

// ToggleExpanded is a no-op: a finished delegation renders as a compact
// summary, and its full result is only reachable by drilling into the
// child session (click, or alt+down) — not by expanding inline. Overriding
// (rather than removing) keeps AgentToolMessageItem satisfying Expandable,
// which HandleDelayedClick and ToggleExpandedSelectedItem both type-assert
// against; a no-op here just means neither ever has anything to do.
func (a *AgentToolMessageItem) ToggleExpanded() bool {
	return false
}

// HoverableAt matches the delegation's whole-item click target.
func (a *AgentToolMessageItem) HoverableAt(x, y, width int) bool {
	return x >= MessageLeftPaddingTotal && y >= 0 && y < lipgloss.Height(a.Render(width))
}

// Animate progresses the message animation if it should be spinning.
//
// Bumps the parent's F6 list-cache version on both the parent-tick and
// nested-tick branches. Nested tools are not list entries of their
// own — their IDs map to this parent's index in idInxMap
// (internal/ui/model/chat.go:240-246) and their renders are embedded
// inline in this parent's output — so the list only checks the
// parent's version. Without the bump, the list cache would serve the
// previously rendered frame indefinitely and the spinner would appear
// frozen.
func (a *AgentToolMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return nil
	}
	if msg.ID == a.ID() {
		a.Bump()
		return a.anim.Animate(msg)
	}
	for _, nestedTool := range a.nestedTools {
		if msg.ID != nestedTool.ID() {
			continue
		}
		if s, ok := nestedTool.(Animatable); ok {
			a.Bump()
			return s.Animate(msg)
		}
	}
	return nil
}

// NestedTools returns the nested tools.
func (a *AgentToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools.
//
// SetNestedTools always bumps the version. The previous design
// deduped when the slice's length and element pointers were
// unchanged, but the live update path in internal/ui/model/ui.go
// mutates existing children in place (SetToolCall / SetResult on the
// same pointers) and then calls SetNestedTools with the same slice.
// Pointer-equality dedupe in that case skips the parent Bump even
// though the parent's rendered output (which embeds the children
// inline) has changed, leaving a stale parent entry in the list
// cache. Always bumping is cheap (one uint64 increment) and called
// at most once per agent event; in the rare case the slice is
// truly unchanged the worst case is one extra parent re-render
// while every child cache hit stays warm.
func (a *AgentToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.clearCache()
	a.Bump()
}

// AddNestedTool adds a nested tool.
func (a *AgentToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.clearCache()
	a.Bump()
}

// AgentToolRenderContext renders agent tool messages.
type AgentToolRenderContext struct {
	agent *AgentToolMessageItem
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgentToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	// ToolCall.Finished only means the model finished streaming the tool
	// arguments. A delegation remains active until its result arrives.
	pending := !opts.HasResult() && !opts.IsCanceled()
	if pending {
		// The session panel owns a running delegation's full live detail
		// (todos, subtitle — see internal/ui/model/session_panel.go and
		// PanelStatusLine below). The chat transcript shows the pending
		// stub (name + spinner) plus one status line underneath with the
		// current activity — elapsed time, step count, last child tool
		// call — so what the task is doing is visible without opening
		// the panel.
		content := pendingDelegation(sty, r.agent.displayName, opts, cappedMessageWidth(width),
			r.agent.startTime, r.agent.nestedTools, r.agent.promptTokens, r.agent.completionTokens)
		return clickableItemHover(sty, content, cappedMessageWidth(width), opts.Hovered)
	}

	cappedWidth := cappedMessageWidth(width)
	var params tools.AgentParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	prompt := params.Prompt

	// runBackgroundAgent (domain/agent/agent_tool.go) returns synchronously
	// with an acknowledgment, not the delegation's actual answer — HasResult
	// is already true the moment this block first renders, well before the
	// background task has done any real work. Detect that case via the
	// result's metadata and render a dedicated "just dispatched" block
	// instead of falling into renderCollapsedDelegation below, which would
	// otherwise show a near-zero duration and treat the ack text as if it
	// were the delegation's finished output.
	if opts.Result != nil {
		var bgMeta tools.AgentBackgroundResponseMetadata
		if err := json.Unmarshal([]byte(opts.Result.Metadata), &bgMeta); err == nil && bgMeta.TaskID != "" {
			content := renderBackgroundDispatch(sty, cappedWidth, r.agent.displayName, opts, prompt, bgMeta)
			return clickableItemHover(sty, content, cappedWidth, opts.Hovered)
		}
	}

	// A finished (or canceled) top-level delegation collapses to a compact
	// summary — the full result and nested-tool tree are only reachable by
	// drilling into the child session (click, or alt+down), never by
	// expanding this block inline. See ToggleExpanded above. Todos are
	// deliberately dropped here (see the todos field doc) — only the
	// model/effort subtitle carries over, since it describes the
	// delegation's configuration rather than its runtime progress.
	if !pending && !opts.Compact {
		content := renderCollapsedDelegation(sty, cappedWidth, r.agent.displayName, opts, prompt, r.agent.nestedTools, r.agent.duration, r.agent.promptTokens, r.agent.completionTokens, r.agent.model, r.agent.effort)
		return clickableItemHover(sty, content, cappedWidth, opts.Hovered)
	}

	prompt = strings.ReplaceAll(prompt, "\n", " ")

	header := toolHeader(sty, opts.Status, r.agent.displayName, cappedWidth, opts)
	if opts.Compact {
		return header
	}

	if subtitle := renderAgentSubtitle(sty, cappedWidth, r.agent.model, r.agent.effort); subtitle != "" {
		header = lipgloss.JoinVertical(lipgloss.Left, header, subtitle)
	}

	// Build the task tag and prompt.
	taskTag := sty.Tool.AgentTaskTag.Render("Task")
	taskTagWidth := lipgloss.Width(taskTag)

	// Calculate remaining width for prompt.
	remainingWidth := min(cappedWidth-taskTagWidth-3, maxTextWidth-taskTagWidth-3) // -3 for spacing

	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			taskTag,
			" ",
			promptText,
		),
	)

	// While still running, surface elapsed time, step count, the most
	// recent child tool call, and the child session's todo list so a long
	// delegation stays legible even before its nested-tool tree grows tall
	// enough to scroll off screen.
	if pending {
		if status := renderAgentStatusLine(sty, cappedWidth, r.agent.startTime, r.agent.nestedTools, r.agent.promptTokens, r.agent.completionTokens); status != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, status)
		}
		if todos := renderChildTodos(sty, cappedWidth, r.agent.todos); todos != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, todos)
		}
	}

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	leading, shown := visibleNestedTools(sty, remainingWidth, r.agent.nestedTools)
	if leading != "" {
		childTools.Child(leading)
	}
	for _, nestedTool := range shown {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, taskTagWidth-5)).String())

	// Show animation if still running.
	if !opts.HasResult() && !opts.IsCanceled() {
		parts = append(parts, "", opts.Anim.Render())
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal)
		return joinToolParts(result, body)
	}

	return clickableItemHover(sty, result, cappedWidth, opts.Hovered)
}

// -----------------------------------------------------------------------------
// Agentic Fetch Tool
// -----------------------------------------------------------------------------

// AgenticFetchToolMessageItem is a message item that represents an agentic fetch tool call.
type AgenticFetchToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem

	// See [AgentToolMessageItem] for the rationale behind these fields.
	// agenticFetchDisplayName is fixed — unlike the agent tool,
	// agentic_fetch has no cfg.Agents entry to name or configure a
	// model/effort override for, so there's no displayName/model/effort
	// trio here.
	startTime        time.Time
	promptTokens     int64
	completionTokens int64
	todos            []session.Todo
	duration         time.Duration
}

var (
	_ ToolMessageItem          = (*AgenticFetchToolMessageItem)(nil)
	_ NestedToolContainer      = (*AgenticFetchToolMessageItem)(nil)
	_ ChildSessionTokenTracker = (*AgenticFetchToolMessageItem)(nil)
	_ ChildSessionTodoTracker  = (*AgenticFetchToolMessageItem)(nil)
	_ DelegationInfoProvider   = (*AgenticFetchToolMessageItem)(nil)
)

// agenticFetchDisplayName is the delegation block's title for the
// agentic_fetch tool — shorter than the tool's own prettified name
// ("Agentic Fetch", used elsewhere e.g. in clipboard copy) to match the
// other delegation blocks' lowercase, single-word titles ("task",
// "developer").
const agenticFetchDisplayName = "fetch"

// DelegationInfo implements [DelegationInfoProvider]. agentic_fetch has no
// cfg.Agents entry, so it never has a model/effort override to report.
func (r *AgenticFetchToolMessageItem) DelegationInfo() (displayName, model, effort string, startTime time.Time, duration time.Duration) {
	return agenticFetchDisplayName, "", "", r.startTime, r.duration
}

var _ PanelLiveActivityProvider = (*AgenticFetchToolMessageItem)(nil)

// PanelStatusLine implements [PanelLiveActivityProvider]. See
// AgentToolMessageItem.PanelStatusLine.
func (r *AgenticFetchToolMessageItem) PanelStatusLine(sty *styles.Styles, width int) string {
	return renderPanelStatusLine(sty, width, r.startTime, r.nestedTools, r.todos, r.promptTokens, r.completionTokens)
}

// NewAgenticFetchToolMessageItem creates a new [AgenticFetchToolMessageItem].
func NewAgenticFetchToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgenticFetchToolMessageItem {
	t := &AgenticFetchToolMessageItem{startTime: time.Now()}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgenticFetchToolRenderContext{fetch: t}, canceled)
	// For the agentic fetch tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
}

// AlwaysSpaced implements list.AlwaysSpaced. Same rationale as
// AgentToolMessageItem.AlwaysSpaced: a delegation is a visually
// significant boundary regardless of its current rendered height.
func (a *AgenticFetchToolMessageItem) AlwaysSpaced() bool {
	return true
}

// SetChildSessionTokens implements [ChildSessionTokenTracker].
func (a *AgenticFetchToolMessageItem) SetChildSessionTokens(prompt, completion int64) {
	if a.promptTokens == prompt && a.completionTokens == completion {
		return
	}
	a.promptTokens = prompt
	a.completionTokens = completion
	a.clearCache()
	a.Bump()
}

// SetChildSessionTodos implements [ChildSessionTodoTracker]. See
// [AgentToolMessageItem.SetChildSessionTodos] for the dedupe rationale.
func (a *AgenticFetchToolMessageItem) SetChildSessionTodos(todos []session.Todo) {
	if slices.Equal(a.todos, todos) {
		return
	}
	a.todos = todos
	a.clearCache()
	a.Bump()
}

// SetResult / SetStatus / ToggleExpanded mirror AgentToolMessageItem's
// overrides — see the doc comments there for the rationale.
func (a *AgenticFetchToolMessageItem) SetResult(res *message.ToolResult) {
	if res != nil && a.duration == 0 {
		a.duration = time.Since(a.startTime)
	}
	a.baseToolMessageItem.SetResult(res)
}

func (a *AgenticFetchToolMessageItem) SetStatus(status ToolStatus) {
	if status == ToolStatusCanceled && a.duration == 0 {
		a.duration = time.Since(a.startTime)
	}
	a.baseToolMessageItem.SetStatus(status)
}

func (a *AgenticFetchToolMessageItem) ToggleExpanded() bool {
	return false
}

// HoverableAt matches the delegation's whole-item click target.
func (a *AgenticFetchToolMessageItem) HoverableAt(x, y, width int) bool {
	return x >= MessageLeftPaddingTotal && y >= 0 && y < lipgloss.Height(a.Render(width))
}

// Animate progresses the message animation if it should be spinning.
// See [AgentToolMessageItem.Animate] for the parent-bump rationale —
// without an override, the embedded base.Animate would (a) drop
// StepMsgs whose ID matches a nested child instead of the parent
// (anim.Animate's ID check at internal/ui/anim/anim.go:326-329
// silently returns nil), and (b) never invalidate the parent's
// list-cache entry on a parent tick.
func (a *AgenticFetchToolMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if a.result != nil || a.Status() == ToolStatusCanceled {
		return nil
	}
	if msg.ID == a.ID() {
		a.Bump()
		return a.anim.Animate(msg)
	}
	for _, nestedTool := range a.nestedTools {
		if msg.ID != nestedTool.ID() {
			continue
		}
		if s, ok := nestedTool.(Animatable); ok {
			a.Bump()
			return s.Animate(msg)
		}
	}
	return nil
}

// NestedTools returns the nested tools.
func (a *AgenticFetchToolMessageItem) NestedTools() []ToolMessageItem {
	return a.nestedTools
}

// SetNestedTools sets the nested tools. Always bumps the version;
// see [AgentToolMessageItem.SetNestedTools] for the rationale.
func (a *AgenticFetchToolMessageItem) SetNestedTools(tools []ToolMessageItem) {
	a.nestedTools = tools
	a.clearCache()
	a.Bump()
}

// AddNestedTool adds a nested tool.
func (a *AgenticFetchToolMessageItem) AddNestedTool(tool ToolMessageItem) {
	// Mark nested tools as simple (compact) rendering.
	if s, ok := tool.(Compactable); ok {
		s.SetCompact(true)
	}
	a.nestedTools = append(a.nestedTools, tool)
	a.clearCache()
	a.Bump()
}

// AgenticFetchToolRenderContext renders agentic fetch tool messages.
type AgenticFetchToolRenderContext struct {
	fetch *AgenticFetchToolMessageItem
}

// agenticFetchParams matches tools.AgenticFetchParams.
type agenticFetchParams struct {
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt"`
}

// RenderTool implements the [ToolRenderer] interface.
func (r *AgenticFetchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	pending := opts.IsPending()
	if pending {
		// See AgentToolRenderContext.RenderTool's matching change: pending
		// stub plus a current-activity status line underneath.
		content := pendingDelegation(sty, agenticFetchDisplayName, opts, cappedMessageWidth(width),
			r.fetch.startTime, r.fetch.nestedTools, r.fetch.promptTokens, r.fetch.completionTokens)
		return clickableItemHover(sty, content, cappedMessageWidth(width), opts.Hovered)
	}

	cappedWidth := cappedMessageWidth(width)
	var params agenticFetchParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	prompt := params.Prompt

	// A finished (or canceled) top-level delegation collapses to a compact
	// summary — see AgentToolRenderContext.RenderTool above for the
	// rationale.
	if !pending && !opts.Compact {
		headerParam := params.URL
		if headerParam == "" {
			headerParam = prompt
		}
		content := renderCollapsedDelegation(sty, cappedWidth, agenticFetchDisplayName, opts, headerParam, r.fetch.nestedTools, r.fetch.duration, r.fetch.promptTokens, r.fetch.completionTokens, "", "")
		return clickableItemHover(sty, content, cappedWidth, opts.Hovered)
	}

	prompt = strings.ReplaceAll(prompt, "\n", " ")

	// Build header with optional URL param.
	var toolParams []string
	if params.URL != "" {
		toolParams = append(toolParams, params.URL)
	}

	header := toolHeader(sty, opts.Status, agenticFetchDisplayName, cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}

	// Build the prompt tag.
	promptTag := sty.Tool.AgenticFetchPromptTag.Render("Prompt")
	promptTagWidth := lipgloss.Width(promptTag)

	// Calculate remaining width for prompt text.
	remainingWidth := min(cappedWidth-promptTagWidth-3, maxTextWidth-promptTagWidth-3) // -3 for spacing

	promptText := sty.Tool.AgentPrompt.Width(remainingWidth).Render(prompt)

	header = lipgloss.JoinVertical(
		lipgloss.Left,
		header,
		"",
		lipgloss.JoinHorizontal(
			lipgloss.Left,
			promptTag,
			" ",
			promptText,
		),
	)

	// While still running, surface elapsed time, step count, the most
	// recent child tool call, and the child session's todo list — see the
	// "Agent" tool's RenderTool above.
	if pending {
		if status := renderAgentStatusLine(sty, cappedWidth, r.fetch.startTime, r.fetch.nestedTools, r.fetch.promptTokens, r.fetch.completionTokens); status != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, status)
		}
		if todos := renderChildTodos(sty, cappedWidth, r.fetch.todos); todos != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, todos)
		}
	}

	// Build tree with nested tool calls.
	childTools := tree.Root(header)

	leading, shown := visibleNestedTools(sty, remainingWidth, r.fetch.nestedTools)
	if leading != "" {
		childTools.Child(leading)
	}
	for _, nestedTool := range shown {
		childView := nestedTool.Render(remainingWidth)
		childTools.Child(childView)
	}

	// Build parts.
	var parts []string
	parts = append(parts, childTools.Enumerator(roundedEnumerator(2, promptTagWidth-5)).String())

	// Show animation if still running.
	if !opts.HasResult() && !opts.IsCanceled() {
		parts = append(parts, "", opts.Anim.Render())
	}

	result := lipgloss.JoinVertical(lipgloss.Left, parts...)

	// Add body content when completed.
	if opts.HasResult() && opts.Result.Content != "" {
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal)
		return joinToolParts(result, body)
	}

	return clickableItemHover(sty, result, cappedWidth, opts.Hovered)
}

// maxVisibleNestedTools caps how many nested tool calls a delegation
// renders inline. A long-running agent/agentic_fetch can accumulate
// dozens of child tool calls; rendering all of them makes the tree grow
// unboundedly tall, so only the most recent ones are shown.
const maxVisibleNestedTools = 3

// visibleNestedTools trims nested to the last maxVisibleNestedTools
// entries for display. When entries are dropped, leading is a single
// "…+N earlier steps" summary line (styled and width-truncated like
// renderAgentStatusLine's status note) meant to be added as the first
// child before shown; it's "" when nothing was dropped. The underlying
// nested slice is never modified — this only affects what gets rendered.
func visibleNestedTools(sty *styles.Styles, width int, nested []ToolMessageItem) (leading string, shown []ToolMessageItem) {
	if len(nested) <= maxVisibleNestedTools {
		return "", nested
	}
	dropped := len(nested) - maxVisibleNestedTools
	note := fmt.Sprintf("…+%d earlier steps", dropped)
	if width > 0 {
		note = ansi.Truncate(note, width, "…")
	}
	leading = sty.Tool.TodoStatusNote.Render(note)
	return leading, nested[len(nested)-maxVisibleNestedTools:]
}

// -----------------------------------------------------------------------------
// Collapsed (finished) delegation block
// -----------------------------------------------------------------------------
//
// Before this existed, a finished agent/agentic_fetch tool call rendered
// exactly like a running one: full nested-tool tree plus the delegation's
// entire result inline. In a long session that's a wall of text for every
// completed delegation — the user asked to drill into the sub-agent's own
// session to inspect it, not to have it permanently expanded in the parent
// chat. renderCollapsedDelegation replaces that with a 2-3 line summary;
// full detail is reached by clicking the block (see enterChildSession in
// internal/ui/model/ui.go), never by expanding it inline — see
// AgentToolMessageItem.ToggleExpanded.

// renderCollapsedDelegation renders a finished (or canceled) delegation as
// a compact block: a header line (status icon, tool name, first line of
// the prompt/URL), an optional model/effort subtitle, an outcome line
// (step count, duration when known, token usage), and — only when the
// delegation produced output — one line previewing the start of the
// result. model/effort are "" for agentic_fetch, which has no per-agent
// config to report.
func renderCollapsedDelegation(
	sty *styles.Styles,
	width int,
	name string,
	opts *ToolRenderOpts,
	headerParam string,
	nestedTools []ToolMessageItem,
	duration time.Duration,
	promptTokens, completionTokens int64,
	model, effort string,
) string {
	lines := []string{toolHeader(sty, opts.Status, name, width, opts, firstLine(headerParam))}

	if subtitle := renderAgentSubtitle(sty, width, model, effort); subtitle != "" {
		lines = append(lines, subtitle)
	}

	if line := renderDelegationOutcomeLine(sty, width, opts.Status, len(nestedTools), duration, promptTokens, completionTokens); line != "" {
		lines = append(lines, line)
	}

	if opts.HasResult() && opts.Result.Content != "" {
		if line := renderResultPreviewLine(sty, width, opts.Result.Content); line != "" {
			lines = append(lines, line)
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

// renderBackgroundDispatch renders a background agent-tool dispatch: the
// tool call itself is finished (it returned an acknowledgment), but the
// background task it started has barely begun — unlike a normal finished
// delegation (renderCollapsedDelegation), there is no real duration to
// report yet and the ack text is not a result worth previewing. The block
// instead shows the delegation's name/goal plus a "background" marker and
// the dispatched task's id, so it reads as "started elsewhere" rather than
// "running" (no spinner — see AgentToolMessageItem's spinningFunc) or
// "finished with an answer". The task id lets a user correlate this block
// with its row in the tasks panel.
func renderBackgroundDispatch(
	sty *styles.Styles,
	width int,
	name string,
	opts *ToolRenderOpts,
	prompt string,
	meta tools.AgentBackgroundResponseMetadata,
) string {
	header := toolHeader(sty, opts.Status, name, width, opts, firstLine(prompt))
	if opts.Compact {
		return header
	}

	// JobToolName is the same "info" token bash.go's renderJobTool uses to
	// mark a background shell job — reused here so both kinds of
	// "started, running independently" work read consistently.
	badge := sty.Tool.JobToolName.Render("background")
	sep := sty.Tool.TodoStatusNote.Render(" · task ")
	taskID := sty.Tool.TodoStatusNote.Render(meta.TaskID)
	note := ansi.Truncate(badge+sep+taskID, width, "…")

	return lipgloss.JoinVertical(lipgloss.Left, header, note)
}

// renderDelegationOutcomeLine renders the collapsed block's second line,
// e.g. `step 12 · 45s · 3.2k tok`. Duration is omitted when unknown (see
// the duration field doc on AgentToolMessageItem) rather than showing a
// misleading value. Returns "" if width leaves no room.
func renderDelegationOutcomeLine(sty *styles.Styles, width int, status ToolStatus, steps int, duration time.Duration, promptTokens, completionTokens int64) string {
	if width <= 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("step %d", steps)}
	if duration > 0 {
		parts = append(parts, presentation.FormatElapsed(duration))
	}
	if total := promptTokens + completionTokens; total > 0 {
		parts = append(parts, presentation.FormatTokenCount(total)+" tok")
	}
	if status == ToolStatusCanceled {
		parts = append(parts, "canceled")
	}
	return sty.Tool.TodoStatusNote.Render(presentation.JoinStatusParts(parts, width))
}

// renderResultPreviewLine renders the collapsed block's third line: the
// first line of the delegation's result, truncated to width. Returns ""
// for empty/whitespace-only content or non-positive width.
func renderResultPreviewLine(sty *styles.Styles, width int, content string) string {
	if width <= 0 {
		return ""
	}
	preview := firstLine(strings.TrimSpace(content))
	if preview == "" {
		return ""
	}
	return sty.Tool.ContentText.Render(ansi.Truncate(preview, width, "…"))
}

// -----------------------------------------------------------------------------
// Running status line
// -----------------------------------------------------------------------------
//
// A delegation (agent/agentic_fetch) can run for many minutes and dozens of
// child tool calls before it says anything back. Before this line existed,
// the only feedback during that stretch was the nested-tool tree — which
// starts out empty and, once populated, only reflects the *last* observed
// child-session pubsub event. If those events are delayed, coalesced, or
// (in client/server mode) briefly interrupted, the render looks frozen even
// though the sub-agent is making progress. renderAgentStatusLine is a
// single line that's cheap to keep fresh every animation tick — most
// importantly, the elapsed-time component advances on wall clock alone, so
// it never stalls even if every other signal does.

// pendingDelegation renders a still-running delegation for the chat
// transcript: the pending stub (status icon, name, spinner) with the
// current-activity status line (elapsed · step N · → last tool · tokens)
// directly underneath, indented to align with the name. Compact (nested)
// renders stay a bare one-line stub.
func pendingDelegation(
	sty *styles.Styles,
	name string,
	opts *ToolRenderOpts,
	width int,
	startTime time.Time,
	nestedTools []ToolMessageItem,
	promptTokens, completionTokens int64,
) string {
	head := pendingTool(sty, name, opts.Anim, opts.Compact)
	if opts.Compact {
		return head
	}
	// The "● " icon prefix is 2 cells wide; indent the status line to sit
	// under the name.
	const indent = "  "
	if status := renderAgentStatusLine(sty, max(0, width-len(indent)), startTime, nestedTools, promptTokens, completionTokens); status != "" {
		head += "\n" + indent + status
	}
	return head
}

// renderAgentStatusLine renders the compact "still running" status line,
// e.g. `4m12s · step 23 · → grep "Provider" internal/config`. Returns "" if
// width leaves no room to render anything useful.
func renderAgentStatusLine(sty *styles.Styles, width int, startTime time.Time, nestedTools []ToolMessageItem, promptTokens, completionTokens int64) string {
	if width <= 0 {
		return ""
	}

	parts := []string{presentation.FormatElapsed(time.Since(startTime)), fmt.Sprintf("step %d", len(nestedTools))}
	if len(nestedTools) > 0 {
		if summary := LastToolSummary(nestedTools[len(nestedTools)-1].ToolCall()); summary != "" {
			parts = append(parts, "→ "+summary)
		}
	}
	if total := promptTokens + completionTokens; total > 0 {
		parts = append(parts, presentation.FormatTokenCount(total)+" tok")
	}

	return sty.Tool.TodoStatusNote.Render(presentation.JoinStatusParts(parts, width))
}

// renderPanelStatusLine is renderAgentStatusLine's counterpart for the
// session panel's delegation block (see PanelLiveActivityProvider): same
// elapsed/step/tokens shape, but the "current activity" segment prefers the
// child session's in-progress todo (its ActiveForm, falling back to
// Content) over the last nested tool call, matching
// childSessionCurrentActivity's own priority in internal/ui/model — a todo
// says more about what's actually happening than a raw tool name.
func renderPanelStatusLine(sty *styles.Styles, width int, startTime time.Time, nestedTools []ToolMessageItem, todos []session.Todo, promptTokens, completionTokens int64) string {
	if width <= 0 {
		return ""
	}

	parts := []string{presentation.FormatElapsed(time.Since(startTime)), fmt.Sprintf("step %d", len(nestedTools))}
	switch {
	case currentTodoActivity(todos) != "":
		parts = append(parts, "→ "+currentTodoActivity(todos))
	case len(nestedTools) > 0:
		if summary := LastToolSummary(nestedTools[len(nestedTools)-1].ToolCall()); summary != "" {
			parts = append(parts, "→ "+summary)
		}
	}
	if total := promptTokens + completionTokens; total > 0 {
		parts = append(parts, presentation.FormatTokenCount(total)+" tok")
	}

	return sty.Tool.TodoStatusNote.Render(presentation.JoinStatusParts(parts, width))
}

// currentTodoActivity returns the in-progress todo's ActiveForm (falling
// back to Content), or "" if none of todos is in progress.
func currentTodoActivity(todos []session.Todo) string {
	for _, t := range todos {
		if t.Status != session.TodoStatusInProgress {
			continue
		}
		if t.ActiveForm != "" {
			return t.ActiveForm
		}
		return t.Content
	}
	return ""
}

// -----------------------------------------------------------------------------
// Model/effort subtitle
// -----------------------------------------------------------------------------
//
// A delegation configured with config.Agent.Model or ReasoningEffort runs on
// a different model/effort than the conversation it's called from — worth
// calling out, since otherwise nothing in the block distinguishes "this
// sub-agent thinks with a cheaper/different model" from the common case of
// just inheriting the app's default. An agent with neither field set is
// exactly that common case, so renderAgentSubtitle renders nothing rather
// than a subtitle that just repeats the parent's own model.

// renderAgentSubtitle renders the delegation's configured model/effort
// override, e.g. "qwen36-local/Qwen3-Coder-Next · effort high". Returns ""
// when both are unset (inherits the app's defaults) or width leaves no
// room. If the full "provider/model-id" string doesn't fit, it retries
// with just the model id (the part after the last "/").
func renderAgentSubtitle(sty *styles.Styles, width int, model, effort string) string {
	if width <= 0 || (model == "" && effort == "") {
		return ""
	}
	var parts []string
	if model != "" {
		parts = append(parts, model)
	}
	if effort != "" {
		parts = append(parts, "effort "+effort)
	}
	line := strings.Join(parts, " · ")
	if model != "" && ansi.StringWidth(line) > width {
		if i := strings.LastIndex(model, "/"); i >= 0 {
			parts[0] = model[i+1:]
			line = strings.Join(parts, " · ")
		}
	}
	return sty.Tool.TodoStatusNote.Render(ansi.Truncate(line, width, "…"))
}

// -----------------------------------------------------------------------------
// Child session todo pane
// -----------------------------------------------------------------------------
//
// A running delegation's todo list is the clearest signal of what it's
// actually doing, beyond "some tool ran recently" (renderAgentStatusLine)
// — so it's surfaced directly under the status line. Only shown while
// running: a finished delegation collapses to a summary (see
// renderCollapsedDelegation / AgentToolMessageItem.ToggleExpanded) and its
// todos, like its nested-tool tree, are only reachable by drilling into the
// child session.

// maxDelegationTodoLines caps how many todo lines the compact per-
// delegation pane renders — mirrors maxVisibleNestedTools's rationale for
// the nested-tool tree.
const maxDelegationTodoLines = 5

// renderChildTodos renders a running child session's todo list, capped to
// maxDelegationTodoLines lines via capTodosForDelegation. Returns "" for an
// empty list or non-positive width.
func renderChildTodos(sty *styles.Styles, width int, todos []session.Todo) string {
	if width <= 0 || len(todos) == 0 {
		return ""
	}
	return FormatTodosList(sty, capTodosForDelegation(todos, maxDelegationTodoLines), styles.ArrowRightIcon, width)
}

// capTodosForDelegation keeps every in-progress item, then fills the
// remaining line budget with pending and completed items in that order. This
// intentionally lets in-progress rows exceed maxLines: hiding active work is
// less useful than preserving the nominal compact-pane cap.
func capTodosForDelegation(todos []session.Todo, maxLines int) []session.Todo {
	buckets := presentation.BucketTodos(todos)
	out := make([]session.Todo, 0, len(todos))
	out = append(out, buckets.InProgress...)
	for _, bucket := range [][]session.Todo{buckets.Pending, buckets.Completed} {
		for _, todo := range bucket {
			if len(out) >= maxLines {
				return out
			}
			out = append(out, todo)
		}
	}
	return out
}

// LastToolSummary describes a single child tool call as "name key-arg" for
// a status line, e.g. `grep "Provider" internal/config`. Falls back to
// just the tool name when there's no argument worth summarizing. Exported
// for the child-session panel in internal/ui/model, which builds the same
// "→ last tool" activity text from the child session's own loaded chat
// while viewing it directly (see Chat.LastToolCall).
func LastToolSummary(tc message.ToolCall) string {
	if arg := toolKeyArgument(tc); arg != "" {
		return tc.Name + " " + arg
	}
	return tc.Name
}

// toolKeyArgument extracts the single most identifying argument from a
// tool call's JSON input — whatever a human would point at to say "it's
// doing X". Unrecognized tools and unparsable input yield "".
func toolKeyArgument(tc message.ToolCall) string {
	switch tc.Name {
	case tools.GrepToolName, tools.RipgrepToolName, tools.GlobToolName:
		var p struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) != nil || p.Pattern == "" {
			return ""
		}
		if p.Path != "" {
			return fmt.Sprintf("%q %s", p.Pattern, p.Path)
		}
		return fmt.Sprintf("%q", p.Pattern)
	case tools.ReadToolName, tools.LegacyReadToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName:
		var p struct {
			FilePath string `json:"file_path"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			return p.FilePath
		}
	case tools.LSToolName:
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			return p.Path
		}
	case tools.BashToolName:
		var p struct {
			Description string `json:"description"`
			Command     string `json:"command"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			if p.Description != "" {
				return p.Description
			}
			return p.Command
		}
	case tools.FetchToolName, tools.WebFetchToolName, tools.AgenticFetchToolName:
		var p struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			return p.URL
		}
	case tools.WebSearchToolName:
		var p struct {
			Query string `json:"query"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			return p.Query
		}
	case tools.AgentToolName:
		var p struct {
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal([]byte(tc.Input), &p) == nil {
			return firstLine(p.Prompt)
		}
	}
	return ""
}

// firstLine returns s up to (not including) its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
