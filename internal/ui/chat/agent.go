package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/ui/anim"
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

// AgentToolMessageItem is a message item that represents an agent tool call.
type AgentToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem

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
)

// NewAgentToolMessageItem creates a new [AgentToolMessageItem].
func NewAgentToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) *AgentToolMessageItem {
	t := &AgentToolMessageItem{startTime: time.Now()}
	t.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgentToolRenderContext{agent: t}, canceled)
	// For the agent tool we keep spinning until the tool call is finished.
	t.spinningFunc = func(state SpinningState) bool {
		return !state.HasResult() && !state.IsCanceled()
	}
	return t
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
	cappedWidth := cappedMessageWidth(width)
	pending := opts.IsPending()
	if pending && len(r.agent.nestedTools) == 0 {
		header := pendingTool(sty, "Agent", opts.Anim, opts.Compact)
		if opts.Compact {
			return header
		}
		// No child tool call has arrived yet — still show elapsed time so
		// the very first seconds of a delegation don't read as frozen.
		if status := renderAgentStatusLine(sty, cappedWidth, r.agent.startTime, nil, r.agent.promptTokens, r.agent.completionTokens); status != "" {
			return lipgloss.JoinVertical(lipgloss.Left, header, status)
		}
		return header
	}

	var params agent.AgentParams
	_ = json.Unmarshal([]byte(opts.ToolCall.Input), &params)

	prompt := params.Prompt

	// A finished (or canceled) top-level delegation collapses to a compact
	// summary — the full result and nested-tool tree are only reachable by
	// drilling into the child session (click, or alt+down), never by
	// expanding this block inline. See ToggleExpanded above.
	if !pending && !opts.Compact {
		return renderCollapsedDelegation(sty, cappedWidth, "Agent", opts, prompt, r.agent.nestedTools, r.agent.duration, r.agent.promptTokens, r.agent.completionTokens)
	}

	if !opts.ExpandedContent {
		prompt = strings.ReplaceAll(prompt, "\n", " ")
	}

	header := toolHeader(sty, opts.Status, "Agent", cappedWidth, opts)
	if opts.Compact {
		return header
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

	// While still running, surface elapsed time, step count, and the most
	// recent child tool call so a long delegation stays legible even
	// before its nested-tool tree grows tall enough to scroll off screen.
	if pending {
		if status := renderAgentStatusLine(sty, cappedWidth, r.agent.startTime, r.agent.nestedTools, r.agent.promptTokens, r.agent.completionTokens); status != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, status)
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
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
}

// -----------------------------------------------------------------------------
// Agentic Fetch Tool
// -----------------------------------------------------------------------------

// AgenticFetchToolMessageItem is a message item that represents an agentic fetch tool call.
type AgenticFetchToolMessageItem struct {
	*baseToolMessageItem

	nestedTools []ToolMessageItem

	// See [AgentToolMessageItem] for the rationale behind these fields.
	startTime        time.Time
	promptTokens     int64
	completionTokens int64
	duration         time.Duration
}

var (
	_ ToolMessageItem          = (*AgenticFetchToolMessageItem)(nil)
	_ NestedToolContainer      = (*AgenticFetchToolMessageItem)(nil)
	_ ChildSessionTokenTracker = (*AgenticFetchToolMessageItem)(nil)
)

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
	cappedWidth := cappedMessageWidth(width)
	pending := opts.IsPending()
	if pending && len(r.fetch.nestedTools) == 0 {
		header := pendingTool(sty, "Agentic Fetch", opts.Anim, opts.Compact)
		if opts.Compact {
			return header
		}
		if status := renderAgentStatusLine(sty, cappedWidth, r.fetch.startTime, nil, r.fetch.promptTokens, r.fetch.completionTokens); status != "" {
			return lipgloss.JoinVertical(lipgloss.Left, header, status)
		}
		return header
	}

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
		return renderCollapsedDelegation(sty, cappedWidth, "Agentic Fetch", opts, headerParam, r.fetch.nestedTools, r.fetch.duration, r.fetch.promptTokens, r.fetch.completionTokens)
	}

	if !opts.ExpandedContent {
		prompt = strings.ReplaceAll(prompt, "\n", " ")
	}

	// Build header with optional URL param.
	var toolParams []string
	if params.URL != "" {
		toolParams = append(toolParams, params.URL)
	}

	header := toolHeader(sty, opts.Status, "Agentic Fetch", cappedWidth, opts, toolParams...)
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

	// While still running, surface elapsed time, step count, and the most
	// recent child tool call — see the "Agent" tool's RenderTool above.
	if pending {
		if status := renderAgentStatusLine(sty, cappedWidth, r.fetch.startTime, r.fetch.nestedTools, r.fetch.promptTokens, r.fetch.completionTokens); status != "" {
			header = lipgloss.JoinVertical(lipgloss.Left, header, status)
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
		body := toolOutputMarkdownContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(result, body)
	}

	return result
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
// the prompt/URL), an outcome line (step count, duration when known, token
// usage), and — only when the delegation produced output — one line
// previewing the start of the result.
func renderCollapsedDelegation(
	sty *styles.Styles,
	width int,
	name string,
	opts *ToolRenderOpts,
	headerParam string,
	nestedTools []ToolMessageItem,
	duration time.Duration,
	promptTokens, completionTokens int64,
) string {
	lines := []string{toolHeader(sty, opts.Status, name, width, opts, firstLine(headerParam))}

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
		parts = append(parts, formatElapsed(duration))
	}
	if total := promptTokens + completionTokens; total > 0 {
		parts = append(parts, formatTokenCount(total)+" tok")
	}
	if status == ToolStatusCanceled {
		parts = append(parts, "canceled")
	}
	return sty.Tool.TodoStatusNote.Render(ansi.Truncate(strings.Join(parts, " · "), width, "…"))
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

// renderAgentStatusLine renders the compact "still running" status line,
// e.g. `4m12s · step 23 · → grep "Provider" internal/config`. Returns "" if
// width leaves no room to render anything useful.
func renderAgentStatusLine(sty *styles.Styles, width int, startTime time.Time, nestedTools []ToolMessageItem, promptTokens, completionTokens int64) string {
	if width <= 0 {
		return ""
	}

	parts := []string{formatElapsed(time.Since(startTime)), fmt.Sprintf("step %d", len(nestedTools))}
	if len(nestedTools) > 0 {
		if summary := lastToolSummary(nestedTools[len(nestedTools)-1].ToolCall()); summary != "" {
			parts = append(parts, "→ "+summary)
		}
	}
	if total := promptTokens + completionTokens; total > 0 {
		parts = append(parts, formatTokenCount(total)+" tok")
	}

	return sty.Tool.TodoStatusNote.Render(ansi.Truncate(strings.Join(parts, " · "), width, "…"))
}

// formatElapsed renders a duration the way a status line wants it: "45s",
// "4m12s", "1h02m" — never a raw time.Duration string.
func formatElapsed(d time.Duration) string {
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

// formatTokenCount renders large token counts compactly ("12.3k").
func formatTokenCount(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// lastToolSummary describes a single child tool call as "name key-arg" for
// the status line, e.g. `grep "Provider" internal/config`. Falls back to
// just the tool name when there's no argument worth summarizing.
func lastToolSummary(tc message.ToolCall) string {
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
	case tools.GrepToolName, tools.GlobToolName:
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
	case tools.ViewToolName, tools.EditToolName, tools.MultiEditToolName, tools.WriteToolName:
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
	case agent.AgentToolName:
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
