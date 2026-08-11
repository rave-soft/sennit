package chat

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/tree"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/diff"
	"github.com/rave-soft/braid/internal/fsext"
	"github.com/rave-soft/braid/internal/hooks"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/stringext"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// responseContextHeight limits the number of lines displayed in tool output.
// Regular tool calls (view/write/edit/bash/grep/...) never show a body at
// all — see appendResultSummary — so in practice this only still bounds the
// still-alive running-delegation preview in agent.go (toolOutputMarkdownContent).
const responseContextHeight = 10

// previewTruncateFormat notes how much of a body was cut off. There is no
// click-to-see-more for tool output — file/command content is not
// something this chat lets you page through — so, unlike
// assistantMessageTruncateFormat (used for the assistant's own message
// text), this never invites a click.
const previewTruncateFormat = "… (%d more lines not shown)"

// toolBodyLeftPaddingTotal represents the padding that should be applied to each tool body
const toolBodyLeftPaddingTotal = 2

// ToolStatus represents the current state of a tool call.
type ToolStatus int

const (
	ToolStatusAwaitingPermission ToolStatus = iota
	ToolStatusRunning
	ToolStatusSuccess
	ToolStatusError
	ToolStatusCanceled
)

// ToolMessageItem represents a tool call message in the chat UI.
type ToolMessageItem interface {
	MessageItem

	ToolCall() message.ToolCall
	SetToolCall(tc message.ToolCall)
	SetResult(res *message.ToolResult)
	MessageID() string
	SetMessageID(id string)
	SetStatus(status ToolStatus)
	Status() ToolStatus
}

// Compactable is an interface for tool items that can render in a compacted mode.
// When compact mode is enabled, tools render as a compact single-line header.
type Compactable interface {
	SetCompact(compact bool)
}

// SpinningState contains the state passed to SpinningFunc for custom spinning logic.
type SpinningState struct {
	ToolCall message.ToolCall
	Result   *message.ToolResult
	Status   ToolStatus
}

// IsCanceled returns true if the tool status is canceled.
func (s *SpinningState) IsCanceled() bool {
	return s.Status == ToolStatusCanceled
}

// HasResult returns true if the result is not nil.
func (s *SpinningState) HasResult() bool {
	return s.Result != nil
}

// SpinningFunc is a function type for custom spinning logic.
// Returns true if the tool should show the spinning animation.
type SpinningFunc func(state SpinningState) bool

// DefaultToolRenderContext implements the default [ToolRenderer] interface.
type DefaultToolRenderContext struct{}

// RenderTool implements the [ToolRenderer] interface.
func (d *DefaultToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	return "TODO: Implement Tool Renderer For: " + opts.ToolCall.Name
}

// ToolRenderOpts contains the data needed to render a tool call.
type ToolRenderOpts struct {
	ToolCall   message.ToolCall
	Result     *message.ToolResult
	Anim       *anim.Anim
	Compact    bool
	IsSpinning bool
	Status     ToolStatus
}

// IsPending returns true if the tool call is still pending (not finished and
// not canceled).
func (o *ToolRenderOpts) IsPending() bool {
	return !o.ToolCall.Finished && !o.IsCanceled()
}

// IsCanceled returns true if the tool status is canceled.
func (o *ToolRenderOpts) IsCanceled() bool {
	return o.Status == ToolStatusCanceled
}

// HasResult returns true if the result is not nil.
func (o *ToolRenderOpts) HasResult() bool {
	return o.Result != nil
}

// HasEmptyResult returns true if the result is nil or has empty content.
func (o *ToolRenderOpts) HasEmptyResult() bool {
	return o.Result == nil || o.Result.Content == ""
}

// ToolRenderer represents an interface for rendering tool calls.
type ToolRenderer interface {
	RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string
}

// ToolRendererFunc is a function type that implements the [ToolRenderer] interface.
type ToolRendererFunc func(sty *styles.Styles, width int, opts *ToolRenderOpts) string

// RenderTool implements the ToolRenderer interface.
func (f ToolRendererFunc) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	return f(sty, width, opts)
}

// baseToolMessageItem represents a tool call message that can be displayed in the UI.
type baseToolMessageItem struct {
	*list.Versioned
	*highlightableMessageItem
	*cachedMessageItem
	*focusableMessageItem

	toolRenderer ToolRenderer
	toolCall     message.ToolCall
	result       *message.ToolResult
	messageID    string
	status       ToolStatus
	// we use this so we can efficiently cache
	// tools that have a capped width (e.x bash.. and others)
	hasCappedWidth bool
	// isCompact indicates this tool should render in compact mode.
	isCompact bool
	// spinningFunc allows tools to override the default spinning logic.
	// If nil, uses the default: !toolCall.Finished && !canceled.
	spinningFunc SpinningFunc

	sty  *styles.Styles
	anim *anim.Anim
}

// newBaseToolMessageItem is the internal constructor for base tool message items.
func newBaseToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	toolRenderer ToolRenderer,
	canceled bool,
) *baseToolMessageItem {
	// we only do full width for diffs (as far as I know)
	hasCappedWidth := toolCall.Name != tools.EditToolName && toolCall.Name != tools.MultiEditToolName

	status := ToolStatusRunning
	if canceled {
		status = ToolStatusCanceled
	}

	v := list.NewVersioned()
	t := &baseToolMessageItem{
		Versioned:                v,
		highlightableMessageItem: defaultHighlighter(sty, v),
		cachedMessageItem:        &cachedMessageItem{},
		focusableMessageItem:     newFocusableMessageItem(v),
		sty:                      sty,
		toolRenderer:             toolRenderer,
		toolCall:                 toolCall,
		result:                   result,
		status:                   status,
		hasCappedWidth:           hasCappedWidth,
	}
	t.anim = anim.New(anim.Settings{
		ID:          toolCall.ID,
		Size:        15,
		GradColorA:  sty.WorkingGradFromColor,
		GradColorB:  sty.WorkingGradToColor,
		LabelColor:  sty.WorkingLabelColor,
		CycleColors: true,
	})

	return t
}

// toolMessageItemFactory constructs a [ToolMessageItem] for one tool name.
type toolMessageItemFactory func(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem

// toolMessageItemFactories maps tool names to the constructor for their
// dedicated renderer. Tool names with no entry here (MCP tools, unrecognized
// tools) fall through to the default chain in NewToolMessageItem.
var toolMessageItemFactories = map[string]toolMessageItemFactory{
	tools.BashToolName:        NewBashToolMessageItem,
	tools.JobOutputToolName:   NewJobOutputToolMessageItem,
	tools.JobKillToolName:     NewJobKillToolMessageItem,
	tools.ViewToolName:        NewViewToolMessageItem,
	tools.WriteToolName:       NewWriteToolMessageItem,
	tools.EditToolName:        NewEditToolMessageItem,
	tools.MultiEditToolName:   NewMultiEditToolMessageItem,
	tools.GlobToolName:        NewGlobToolMessageItem,
	tools.GrepToolName:        NewGrepToolMessageItem,
	tools.RipgrepToolName:     NewRipgrepToolMessageItem,
	tools.LSToolName:          NewLSToolMessageItem,
	tools.DownloadToolName:    NewDownloadToolMessageItem,
	tools.FetchToolName:       NewFetchToolMessageItem,
	tools.DiagnosticsToolName: NewDiagnosticsToolMessageItem,
	// agent.AgentToolName is deliberately absent here: NewAgentToolMessageItem
	// now takes a cfg argument (to resolve the delegation's display name and
	// any model/effort override, see agentDisplayName), which this factory
	// signature has no room for — NewToolMessageItem special-cases it below
	// instead, alongside the isCustomAgentTool branch that needs the same cfg.
	tools.AgenticFetchToolName: func(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem {
		return NewAgenticFetchToolMessageItem(sty, toolCall, result, canceled)
	},
	tools.WebFetchToolName:      NewWebFetchToolMessageItem,
	tools.WebSearchToolName:     NewWebSearchToolMessageItem,
	tools.TodosToolName:         NewTodosToolMessageItem,
	tools.QuestionToolName:      NewQuestionToolMessageItem,
	tools.ReferencesToolName:    NewReferencesToolMessageItem,
	tools.DefinitionToolName:    NewDefinitionToolMessageItem,
	tools.RenameToolName:        NewRenameToolMessageItem,
	tools.ReplaceSymbolToolName: NewReplaceSymbolToolMessageItem,
	tools.CallHierarchyToolName: NewCallHierarchyToolMessageItem,
	tools.SymbolsToolName:       NewSymbolsToolMessageItem,
	tools.LSPRestartToolName:    NewLSPRestartToolMessageItem,
}

// NewToolMessageItem creates a new [ToolMessageItem] based on the tool call name.
//
// It returns a specific tool message item type if implemented, otherwise it
// returns a generic tool message item. The messageID is the ID of the assistant
// message containing this tool call. cfg is used to recognize user-defined
// agent tools (see isCustomAgentTool) so they get the same renderer as the
// built-in "agent" tool; it may be nil, in which case no tool name is
// treated as a custom agent.
func NewToolMessageItem(
	sty *styles.Styles,
	messageID string,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
	cfg *config.Config,
) ToolMessageItem {
	var item ToolMessageItem
	switch {
	case toolCall.Name == agent.AgentToolName:
		item = NewAgentToolMessageItem(sty, toolCall, result, canceled, cfg)
	case toolMessageItemFactories[toolCall.Name] != nil:
		item = toolMessageItemFactories[toolCall.Name](sty, toolCall, result, canceled)
	case IsDockerMCPTool(toolCall.Name):
		item = NewDockerMCPToolMessageItem(sty, toolCall, result, canceled)
	case strings.HasPrefix(toolCall.Name, "mcp_"):
		item = NewMCPToolMessageItem(sty, toolCall, result, canceled)
	case isCustomAgentTool(cfg, toolCall.Name):
		// User-defined agents (.braid/agents, config.Agents) are
		// delegations of the same shape as the built-in "agent" tool —
		// CustomAgentParams mirrors AgentParams, just a "prompt" field —
		// so they get the identical renderer: running status line,
		// collapse-to-summary once finished, click-to-drill into the
		// child session.
		item = NewAgentToolMessageItem(sty, toolCall, result, canceled, cfg)
	default:
		item = NewGenericToolMessageItem(sty, toolCall, result, canceled)
	}
	item.SetMessageID(messageID)
	return item
}

// isCustomAgentTool reports whether name is a user-defined agent tool.
// internal/agent/custom_agent_tool.go registers one delegation tool per
// entry in cfg.Agents, named after the agent's id — excluding "coder" and
// "task", which are the built-in roles rather than tools a model can call.
func isCustomAgentTool(cfg *config.Config, name string) bool {
	if cfg == nil || name == config.AgentCoder || name == config.AgentTask {
		return false
	}
	_, ok := cfg.Agents[name]
	return ok
}

// SetCompact implements the Compactable interface.
func (t *baseToolMessageItem) SetCompact(compact bool) {
	if t.isCompact == compact {
		return
	}
	t.isCompact = compact
	t.clearCache()
	t.Bump()
}

// ID returns the unique identifier for this tool message item.
func (t *baseToolMessageItem) ID() string {
	return t.toolCall.ID
}

// StartAnimation starts the assistant message animation if it should be spinning.
func (t *baseToolMessageItem) StartAnimation() tea.Cmd {
	if !t.isSpinning() {
		return nil
	}
	return t.anim.Start()
}

// Animate progresses the assistant message animation if it should be spinning.
//
// Bumps the F6 list-cache version so the next draw re-renders this
// item: a spinner tick mutates anim's internal frame counter, which
// changes the rendered output but is invisible to the per-item
// caches. Without the bump the list cache would serve the previously
// rendered frame indefinitely and the spinner would appear frozen.
// The ID gate keeps unrelated ticks (routed here by a future change
// to chat.Animate's dispatch) from churning the cache.
func (t *baseToolMessageItem) Animate(msg anim.StepMsg) tea.Cmd {
	if !t.isSpinning() {
		return nil
	}
	if msg.ID != t.toolCall.ID {
		return nil
	}
	t.Bump()
	return t.anim.Animate(msg)
}

// RawRender implements [MessageItem].
func (t *baseToolMessageItem) RawRender(width int) string {
	toolItemWidth := width - MessageLeftPaddingTotal
	if t.hasCappedWidth {
		toolItemWidth = cappedMessageWidth(width)
	}

	content, height, ok := t.getCachedRender(toolItemWidth)
	// if we are spinning or there is no cache rerender
	if !ok || t.isSpinning() {
		content = t.toolRenderer.RenderTool(t.sty, toolItemWidth, &ToolRenderOpts{
			ToolCall:   t.toolCall,
			Result:     t.result,
			Anim:       t.anim,
			Compact:    t.isCompact,
			IsSpinning: t.isSpinning(),
			Status:     t.computeStatus(),
		})

		// Prepend hook indicator if hooks ran for this tool call.
		if t.result != nil {
			if hookLine := toolOutputHookIndicator(t.sty, t.result.Metadata, toolItemWidth); hookLine != "" {
				content = hookLine + "\n\n" + content
			}
		}

		height = lipgloss.Height(content)
		// cache the rendered content
		t.setCachedRender(content, toolItemWidth, height)
	}

	return t.renderHighlighted(content, toolItemWidth, height)
}

// Render renders the tool message item at the given width.
func (t *baseToolMessageItem) Render(width int) string {
	// Cache the prefixed output keyed by (width, prefix variant).
	// Bypass the cache while spinning (RawRender output is
	// frame-dependent) or while a highlight range is active.
	useCache := !t.isSpinning() && !t.isHighlighted()
	var key uint64
	switch {
	case t.isCompact:
		key = 2
	case t.focused:
		key = 1
	default:
		key = 0
	}
	if useCache {
		if cached, ok := t.getCachedPrefixedRender(width, key); ok {
			return cached
		}
	}
	var prefix string
	if t.isCompact {
		prefix = t.sty.Messages.ToolCallCompact.Render()
	} else if t.focused {
		prefix = t.sty.Messages.ToolCallFocused.Render()
	} else {
		prefix = t.sty.Messages.ToolCallBlurred.Render()
	}
	lines := strings.Split(t.RawRender(width), "\n")
	for i, ln := range lines {
		lines[i] = prefix + ln
	}
	out := strings.Join(lines, "\n")
	if useCache {
		t.setCachedPrefixedRender(out, width, key)
	}
	return out
}

// ToolCall returns the tool call associated with this message item.
func (t *baseToolMessageItem) ToolCall() message.ToolCall {
	return t.toolCall
}

// SetToolCall sets the tool call associated with this message item.
func (t *baseToolMessageItem) SetToolCall(tc message.ToolCall) {
	t.toolCall = tc
	t.clearCache()
	t.Bump()
}

// SetResult sets the tool result associated with this message item.
func (t *baseToolMessageItem) SetResult(res *message.ToolResult) {
	t.result = res
	t.clearCache()
	t.Bump()
}

// MessageID returns the ID of the message containing this tool call.
func (t *baseToolMessageItem) MessageID() string {
	return t.messageID
}

// SetMessageID sets the ID of the message containing this tool call.
// MessageID is metadata only and does not affect the rendered output,
// so we deliberately do not bump the version here.
func (t *baseToolMessageItem) SetMessageID(id string) {
	t.messageID = id
}

// SetStatus sets the tool status.
func (t *baseToolMessageItem) SetStatus(status ToolStatus) {
	if t.status == status {
		return
	}
	t.status = status
	t.clearCache()
	t.Bump()
}

// Status returns the current tool status.
func (t *baseToolMessageItem) Status() ToolStatus {
	return t.status
}

// computeStatus computes the effective status considering the result.
func (t *baseToolMessageItem) computeStatus() ToolStatus {
	if t.result != nil {
		if t.result.IsError {
			return ToolStatusError
		}
		return ToolStatusSuccess
	}
	return t.status
}

// isSpinning returns true if the tool should show animation.
func (t *baseToolMessageItem) isSpinning() bool {
	if t.spinningFunc != nil {
		return t.spinningFunc(SpinningState{
			ToolCall: t.toolCall,
			Result:   t.result,
			Status:   t.status,
		})
	}
	return !t.toolCall.Finished && t.status != ToolStatusCanceled
}

// SetSpinningFunc sets a custom function to determine if the tool should spin.
func (t *baseToolMessageItem) SetSpinningFunc(fn SpinningFunc) {
	t.spinningFunc = fn
}

// Finished implements list.Item. A tool call is freezable once the
// tool call itself is marked finished AND a result has been recorded
// (or it has been canceled). Tools that override the spinning logic
// via spinningFunc would short-circuit live ticks; we still gate
// freezing on isSpinning to keep the contract conservative.
func (t *baseToolMessageItem) Finished() bool {
	if t.isSpinning() {
		return false
	}
	if t.status == ToolStatusCanceled {
		return true
	}
	return t.toolCall.Finished && t.result != nil
}

// HandleMouseClick implements MouseClickable. A left click is reported as
// handled so a click on an agent/agentic_fetch delegation still drills into
// its child session (see NestedToolContainer in model/chat.go's
// HandleDelayedClick) — plain tool items no longer implement Expandable, so
// for them this is otherwise inert: no preview, no toggle. File/command
// content is not something this chat lets you page through.
func (t *baseToolMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	return btn == ansi.MouseLeft
}

// HandleKeyEvent implements KeyEventHandler.
func (t *baseToolMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text := t.formatToolForCopy()
		return true, common.CopyToClipboard(text, "Tool content copied to clipboard")
	}
	return false, nil
}

// pendingTool renders a tool that is still in progress with an animation.
func pendingTool(sty *styles.Styles, name string, anim *anim.Anim, nested bool) string {
	icon := sty.Tool.IconPending.Render()
	nameStyle := sty.Tool.NameNormal
	if nested {
		nameStyle = sty.Tool.NameNested
	}
	toolName := nameStyle.Render(name)

	var animView string
	if anim != nil {
		animView = anim.Render()
	}

	return fmt.Sprintf("%s %s %s", icon, toolName, animView)
}

// toolEarlyStateContent handles error/cancelled/pending states before content rendering.
// Returns the rendered output and true if early state was handled.
func toolEarlyStateContent(sty *styles.Styles, opts *ToolRenderOpts, width int) (string, bool) {
	var msg string
	switch opts.Status {
	case ToolStatusError:
		msg = toolErrorContent(sty, opts.Result, width)
	case ToolStatusCanceled:
		msg = sty.Tool.StateCancelled.Render("Canceled.")
	case ToolStatusAwaitingPermission:
		msg = sty.Tool.StateWaiting.Render("Requesting permission...")
	case ToolStatusRunning:
		msg = sty.Tool.StateWaiting.Render("Waiting for tool response...")
	default:
		return "", false
	}
	return msg, true
}

// toolErrorContent formats an error message with an ERROR or WARN tag.
func toolErrorContent(sty *styles.Styles, result *message.ToolResult, width int) string {
	if result == nil {
		return ""
	}
	errContent := strings.ReplaceAll(result.Content, "\n", " ")
	if strings.Contains(errContent, "User denied permission") ||
		strings.Contains(errContent, "User cancelled") {
		deniedTag := sty.Tool.WarnTag.Render("WARN")
		deniedTagWidth := lipgloss.Width(deniedTag)
		errContent = ansi.Truncate(errContent, width-deniedTagWidth-3, "…")
		return fmt.Sprintf("%s %s", deniedTag, sty.Tool.WarnMessage.Render(errContent))
	}
	errTag := sty.Tool.ErrorTag.Render("ERROR")
	tagWidth := lipgloss.Width(errTag)
	errContent = ansi.Truncate(errContent, width-tagWidth-3, "…")
	return fmt.Sprintf("%s %s", errTag, sty.Tool.ErrorMessage.Render(errContent))
}

// toolIcon returns the status icon for a tool call.
// toolIcon returns the status icon for a tool call based on its status.
func toolIcon(sty *styles.Styles, status ToolStatus) string {
	switch status {
	case ToolStatusSuccess:
		return sty.Tool.IconSuccess.String()
	case ToolStatusError:
		return sty.Tool.IconError.String()
	case ToolStatusCanceled:
		return sty.Tool.IconCancelled.String()
	default:
		return sty.Tool.IconPending.String()
	}
}

// oneLine collapses any run of whitespace — embedded newlines, tabs, CRLF,
// repeated spaces — into a single space, and trims the ends. Tool params
// routinely carry multi-line values (a bash command built from a
// heredoc/multi-line script, a description, an MCP argument) that a caller
// didn't already flatten; toolParamList applies this to every param before
// truncating so a header stays provably one line — ansi.Truncate only
// prevents *horizontal* overflow, it has no idea embedded "\n"s exist.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// toolParamList formats tool parameters as "main (key=value, ...)",
// truncated to width — a tool header is always a single line, so params
// never wrap.
func toolParamList(sty *styles.Styles, params []string, width int) string {
	// minSpaceForMainParam is the min space required for the main param
	// if this is less that the value set we will only show the main param nothing else
	const minSpaceForMainParam = 30
	if len(params) == 0 {
		return ""
	}

	mainParam := oneLine(params[0])

	// Build key=value pairs from remaining params (consecutive key, value pairs).
	var kvPairs []string
	for i := 1; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			kvPairs = append(kvPairs, fmt.Sprintf("%s=%s", params[i], oneLine(params[i+1])))
		}
	}

	// Try to include key=value pairs if there's enough space.
	output := mainParam
	if len(kvPairs) > 0 {
		partsStr := strings.Join(kvPairs, ", ")
		if remaining := width - lipgloss.Width(partsStr) - 3; remaining >= minSpaceForMainParam {
			output = fmt.Sprintf("%s (%s)", mainParam, partsStr)
		}
	}

	if width >= 0 {
		output = ansi.Truncate(output, width, "…")
	}
	return sty.Tool.ParamMain.Render(output)
}

// toolHeader builds the tool header line: "● ToolName params...". Always a
// single line — long parameters truncate, never wrap.
func toolHeader(sty *styles.Styles, status ToolStatus, name string, width int, opts *ToolRenderOpts, params ...string) string {
	nested := opts != nil && opts.Compact
	icon := toolIcon(sty, status)
	nameStyle := sty.Tool.NameNormal
	if nested {
		nameStyle = sty.Tool.NameNested
	}
	toolName := nameStyle.Render(name)
	prefix := fmt.Sprintf("%s %s ", icon, toolName)
	remainingWidth := width - lipgloss.Width(prefix)
	return prefix + toolParamList(sty, params, remainingWidth)
}

// junkPlaceholders lists values that mean "nothing" in a model's own
// vocabulary rather than this codebase's — a model filling an optional
// field (most commonly a "description" param) with the literal text
// "None"/"null"/"n/a" instead of leaving it empty is a well-known
// artifact. isJunkText/cleanDescription treat these exactly like "": a
// summary/description this hollow is worse than no summary at all, since
// it reads as real data.
var junkPlaceholders = map[string]bool{
	"none": true, "null": true, "nil": true, "n/a": true, "na": true,
	"undefined": true, "-": true,
}

// isJunkText reports whether s is empty or one of junkPlaceholders
// (case-insensitively, ignoring surrounding whitespace).
func isJunkText(s string) bool {
	return junkPlaceholders[strings.ToLower(strings.TrimSpace(s))]
}

// cleanDescription returns s, or "" if s is a junk placeholder (see
// isJunkText) — for use with cmp.Or(cleanDescription(modelSupplied),
// fallback) so a placeholder value falls through to the fallback instead
// of being displayed as if it were a real description.
func cleanDescription(s string) string {
	if isJunkText(s) {
		return ""
	}
	return s
}

// appendResultSummary appends a short " · outcome" suffix to a collapsed
// tool's header line — e.g. "342 lines", "+12 −3", "27 matches" — so the
// single default line still says what happened without showing any
// content. Returns header unchanged when summary is "" or a junk
// placeholder (see isJunkText) — never prints a value that means nothing.
func appendResultSummary(sty *styles.Styles, header, summary string) string {
	if summary == "" || isJunkText(summary) {
		return header
	}
	return header + " " + sty.Tool.TodoStatusNote.Render("· "+summary)
}

// lineCountSummary reports content's line count for a collapsed header,
// e.g. "342 lines". Returns "" for empty content.
func lineCountSummary(content string) string {
	if content == "" {
		return ""
	}
	n := strings.Count(content, "\n") + 1
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// diffSummary reports a +additions/−removals count for a collapsed diff
// header, e.g. "+12 −3". Returns "" when there's nothing to report.
func diffSummary(additions, removals int) string {
	if additions == 0 && removals == 0 {
		return ""
	}
	return fmt.Sprintf("+%d −%d", additions, removals)
}

// countSummary reports a labeled count for a collapsed header, e.g.
// "27 matches" or "4 files". Returns "" for a non-positive count.
func countSummary(n int, singular, plural string) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// toolOutputPlainContent renders plain text, capped to responseContextHeight
// lines. There is no click-to-expand for tool bodies (see
// appendResultSummary) — the only remaining caller of this is
// toolOutputMarkdownContent's parse-error fallback, itself only reachable
// from the still-alive running-delegation preview in agent.go.
func toolOutputPlainContent(sty *styles.Styles, content string, width int) string {
	content = stringext.NormalizeSpace(content)
	content = common.StripCursorControl(content)
	content = common.RemapANSI16(content, sty.ANSI)
	lines := strings.Split(content, "\n")

	maxLines := min(responseContextHeight, len(lines))

	var out []string
	for i, ln := range lines {
		if i >= maxLines {
			break
		}
		ln = " " + ln
		if lipgloss.Width(ln) > width {
			ln = ansi.Truncate(ln, width, "…")
		}
		out = append(out, sty.Tool.ContentLine.Width(width).Render(ln))
	}

	if len(lines) > maxLines {
		out = append(out, sty.Tool.ContentTruncation.
			Width(width).
			Render(fmt.Sprintf(previewTruncateFormat, len(lines)-maxLines)))
	}

	return strings.Join(out, "\n")
}

// toolOutputImageContent renders image data with size info.
func toolOutputImageContent(sty *styles.Styles, data, mediaType string) string {
	dataSize := len(data) * 3 / 4
	sizeStr := formatSize(dataSize)

	return sty.Tool.Body.Render(fmt.Sprintf(
		"%s %s %s %s",
		sty.Tool.ResourceLoadedText.Render("Loaded Image"),
		sty.Tool.ResourceLoadedIndicator.Render(styles.ArrowRightIcon),
		sty.Tool.MediaType.Render(mediaType),
		sty.Tool.ResourceSize.Render(sizeStr),
	))
}

// toolOutputSkillContent renders a skill loaded indicator.
func toolOutputSkillContent(sty *styles.Styles, name, description string) string {
	return sty.Tool.Body.Render(fmt.Sprintf(
		"%s %s %s %s",
		sty.Tool.ResourceLoadedText.Render("Loaded Skill"),
		sty.Tool.ResourceLoadedIndicator.Render(styles.ArrowRightIcon),
		sty.Tool.ResourceName.Render(name),
		sty.Tool.ResourceSize.Render(description),
	))
}

// toolOutputHookIndicator renders hook indicator lines from tool metadata.
// Returns empty string if no hook metadata is present. Hook names are
// sanitized (newlines replaced with ¶) and truncated to fit the available
// horizontal space.
func toolOutputHookIndicator(sty *styles.Styles, metadata string, width int) string {
	if metadata == "" {
		return ""
	}
	var meta struct {
		Hook *hooks.HookMetadata `json:"hook"`
	}
	if err := json.Unmarshal([]byte(metadata), &meta); err != nil || meta.Hook == nil {
		return ""
	}
	h := meta.Hook
	if len(h.Hooks) == 0 {
		return ""
	}

	// Sanitize names (replace newlines with ¶) and compute max widths
	// for the name, matcher, and detail columns so they align. The name
	// column is capped at maxHookNameWidth characters.
	const maxHookNameWidth = 30
	sanitizedNames := make([]string, len(h.Hooks))
	details := make([]string, len(h.Hooks))
	maxNameWidth := 0
	maxMatcherWidth := 0
	maxDetailWidth := 0
	for i, hi := range h.Hooks {
		sanitizedNames[i] = strings.ReplaceAll(hi.Name, "\n", "¶")
		w := lipgloss.Width(sty.Tool.HookName.Render(sanitizedNames[i]))
		if w > maxNameWidth {
			maxNameWidth = w
		}
		if hi.Matcher != "" {
			mw := lipgloss.Width(sty.Tool.HookMatcher.Render(hi.Matcher))
			if mw > maxMatcherWidth {
				maxMatcherWidth = mw
			}
		}
		details[i] = hookDetail(sty, hi)
		if dw := lipgloss.Width(details[i]); dw > maxDetailWidth {
			maxDetailWidth = dw
		}
	}

	if maxNameWidth > maxHookNameWidth {
		maxNameWidth = maxHookNameWidth
	}

	// Cap the name column so the widest line still fits in width. The
	// per-line layout is:
	//   "Hook " + name(padded) + [" " + matcher(padded)] + " → " + detail
	if width > 0 {
		fixed := lipgloss.Width(sty.Tool.HookLabel.Render("Hook")) + 1
		if maxMatcherWidth > 0 {
			fixed += 1 + maxMatcherWidth
		}
		fixed += 1 + lipgloss.Width(sty.Tool.HookArrow.Render(styles.ArrowRightIcon)) + 1
		fixed += maxDetailWidth
		if budget := width - fixed; budget < maxNameWidth {
			maxNameWidth = max(1, budget)
		}
	}

	var lines []string
	for i, hi := range h.Hooks {
		name := truncateHookName(sanitizedNames[i], maxNameWidth)
		lines = append(lines, renderHookLine(sty, hi, name, details[i], maxNameWidth, maxMatcherWidth))
	}
	return strings.Join(lines, "\n")
}

// truncateHookName truncates a hook name to fit within maxWidth cells,
// using left-truncation for absolute paths (e.g. `…/format.sh`) and
// right-truncation for everything else. Left-truncation is only applied
// when the name looks unambiguously like a path: absolute, single-line,
// and contains no spaces.
func truncateHookName(name string, maxWidth int) string {
	if ansi.StringWidth(name) <= maxWidth {
		return name
	}
	if isLikelyPath(name) {
		// ansi.TruncateLeft removes n graphemes from the start; pick n
		// so the result plus the "…" prefix fits in maxWidth.
		n := ansi.StringWidth(name) - maxWidth + 1
		return ansi.TruncateLeft(name, n, "…")
	}
	return ansi.Truncate(name, maxWidth, "…")
}

// isLikelyPath reports whether s looks unambiguously like a filesystem
// path, suitable for left-truncation. We accept absolute paths and
// relative paths that contain a separator and no shell-ish characters.
func isLikelyPath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \t\n¶'\"|&;<>$`*?(){}[]\\") {
		return false
	}
	if filepath.IsAbs(s) {
		return true
	}
	return strings.Contains(s, "/")
}

// renderHookLine renders a single hook indicator line with aligned columns.
func renderHookLine(sty *styles.Styles, hi hooks.HookInfo, rawName, detail string, maxNameWidth, maxMatcherWidth int) string {
	name := sty.Tool.HookName.Render(rawName)
	namePad := strings.Repeat(" ", max(0, maxNameWidth-lipgloss.Width(name)))

	var matcherPart string
	if maxMatcherWidth > 0 {
		if hi.Matcher != "" {
			matcher := sty.Tool.HookMatcher.Render(hi.Matcher)
			matcherPad := strings.Repeat(" ", maxMatcherWidth-lipgloss.Width(matcher))
			matcherPart = " " + matcher + matcherPad
		} else {
			matcherPart = " " + strings.Repeat(" ", maxMatcherWidth)
		}
	}

	labelStyle := sty.Tool.HookLabel
	arrowStyle := sty.Tool.HookArrow
	if hi.Decision == "deny" {
		labelStyle = sty.Tool.HookDeniedLabel
		arrowStyle = sty.Tool.HookDeniedLabel
	}

	return fmt.Sprintf(
		"%s %s%s%s %s %s",
		labelStyle.Render("Hook"),
		name,
		namePad,
		matcherPart,
		arrowStyle.Render(styles.ArrowRightIcon),
		detail,
	)
}

// hookDetail returns the styled detail text for a single hook result.
func hookDetail(sty *styles.Styles, hi hooks.HookInfo) string {
	const (
		okMessage      = "OK"
		denialMessage  = "Denied"
		rewroteMessage = "Rewrote Output"
	)
	switch hi.Decision {
	case "deny":
		if hi.Reason != "" {
			return sty.Tool.HookDenied.Render(denialMessage) + " " + sty.Tool.HookDeniedReason.Render(hi.Reason)
		}
		return sty.Tool.HookDenied.Render(denialMessage)
	case "allow":
		result := sty.Tool.HookOK.Render(okMessage)
		if hi.InputRewrite {
			result += " " + sty.Tool.HookRewrote.Render(rewroteMessage)
		}
		return result
	default:
		result := sty.Tool.HookOK.Render(okMessage)
		if hi.InputRewrite {
			result += " " + sty.Tool.HookRewrote.Render(rewroteMessage)
		}
		return result
	}
}

// getDigits returns the number of digits in a number.
func getDigits(n int) int {
	if n == 0 {
		return 1
	}
	if n < 0 {
		n = -n
	}
	digits := 0
	for n > 0 {
		n /= 10
		digits++
	}
	return digits
}

// formatSize formats byte size into human readable format.
func formatSize(bytes int) string {
	const (
		kb = 1024
		mb = kb * 1024
	)
	switch {
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	case bytes >= kb:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(kb))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// formatTimeout converts timeout seconds to a duration string (e.g., "30s").
// Returns empty string if timeout is 0.
func formatTimeout(timeout int) string {
	if timeout == 0 {
		return ""
	}
	return fmt.Sprintf("%ds", timeout)
}

// formatNonZero returns string representation of non-zero integers, empty string for zero.
func formatNonZero(value int) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}

// roundedEnumerator creates a tree enumerator with rounded corners.
func roundedEnumerator(lPadding, width int) tree.Enumerator {
	if width == 0 {
		width = 2
	}
	if lPadding == 0 {
		lPadding = 1
	}
	return func(children tree.Children, index int) string {
		line := strings.Repeat("─", width)
		padding := strings.Repeat(" ", lPadding)
		if children.Length()-1 == index {
			return padding + "╰" + line
		}
		return padding + "├" + line
	}
}

// toolOutputMarkdownContent renders markdown content, capped to
// responseContextHeight lines. Used only by agent.go for the still-alive
// running-delegation preview — no per-tool result body renders through
// this anymore (see appendResultSummary).
func toolOutputMarkdownContent(sty *styles.Styles, content string, width int) string {
	content = stringext.NormalizeSpace(content)

	// Cap width for readability.
	if width > maxTextWidth {
		width = maxTextWidth
	}

	renderer := common.QuietMarkdownRenderer(sty, width)
	mu := common.LockMarkdownRenderer(renderer)
	mu.Lock()
	rendered, err := renderer.Render(content)
	mu.Unlock()
	if err != nil {
		return toolOutputPlainContent(sty, content, width)
	}

	lines := strings.Split(rendered, "\n")
	maxLines := min(responseContextHeight, len(lines))

	var out []string
	for i, ln := range lines {
		if i >= maxLines {
			break
		}
		out = append(out, ln)
	}

	if len(lines) > maxLines {
		out = append(
			out, sty.Tool.ContentTruncation.
				Width(width).
				Render(fmt.Sprintf(previewTruncateFormat, len(lines)-maxLines)),
		)
	}

	return sty.Tool.Body.Render(strings.Join(out, "\n"))
}

// formatToolForCopy formats the tool call for clipboard copying.
func (t *baseToolMessageItem) formatToolForCopy() string {
	var parts []string

	toolName := prettifyToolName(t.toolCall.Name)
	parts = append(parts, fmt.Sprintf("## %s Tool Call", toolName))

	if t.toolCall.Input != "" {
		params := t.formatParametersForCopy()
		if params != "" {
			parts = append(parts, "### Parameters:")
			parts = append(parts, params)
		}
	}

	if t.result != nil && t.result.ToolCallID != "" {
		if t.result.IsError {
			parts = append(parts, "### Error:")
			parts = append(parts, t.result.Content)
		} else {
			parts = append(parts, "### Result:")
			content := t.formatResultForCopy()
			if content != "" {
				parts = append(parts, content)
			}
		}
	} else if t.status == ToolStatusCanceled {
		parts = append(parts, "### Status:")
		parts = append(parts, "Cancelled")
	} else {
		parts = append(parts, "### Status:")
		parts = append(parts, "Pending...")
	}

	return strings.Join(parts, "\n\n")
}

// formatParametersForCopy formats tool parameters for clipboard copying.
func (t *baseToolMessageItem) formatParametersForCopy() string {
	switch t.toolCall.Name {
	case tools.BashToolName:
		var params tools.BashParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			cmd := strings.ReplaceAll(params.Command, "\n", " ")
			cmd = strings.ReplaceAll(cmd, "\t", "    ")
			return fmt.Sprintf("**Command:** %s", cmd)
		}
	case tools.ViewToolName:
		var params tools.ViewParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath)))
			if params.Limit > 0 {
				parts = append(parts, fmt.Sprintf("**Limit:** %d", params.Limit))
			}
			if params.Offset > 0 {
				parts = append(parts, fmt.Sprintf("**Offset:** %d", params.Offset))
			}
			return strings.Join(parts, "\n")
		}
	case tools.EditToolName:
		var params tools.EditParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath))
		}
	case tools.MultiEditToolName:
		var params tools.MultiEditParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath)))
			parts = append(parts, fmt.Sprintf("**Edits:** %d", len(params.Edits)))
			return strings.Join(parts, "\n")
		}
	case tools.WriteToolName:
		var params tools.WriteParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**File:** %s", fsext.PrettyPath(params.FilePath))
		}
	case tools.FetchToolName:
		var params tools.FetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			if params.Format != "" {
				parts = append(parts, fmt.Sprintf("**Format:** %s", params.Format))
			}
			if params.Timeout > 0 {
				parts = append(parts, fmt.Sprintf("**Timeout:** %ds", params.Timeout))
			}
			return strings.Join(parts, "\n")
		}
	case tools.AgenticFetchToolName:
		var params tools.AgenticFetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			if params.URL != "" {
				parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			}
			if params.Prompt != "" {
				parts = append(parts, fmt.Sprintf("**Prompt:** %s", params.Prompt))
			}
			return strings.Join(parts, "\n")
		}
	case tools.WebFetchToolName:
		var params tools.WebFetchParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**URL:** %s", params.URL)
		}
	case tools.GrepToolName, tools.RipgrepToolName:
		var params tools.RipgrepParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**Pattern:** %s", params.Pattern))
			if params.Path != "" {
				parts = append(parts, fmt.Sprintf("**Path:** %s", params.Path))
			}
			if params.Include != "" {
				parts = append(parts, fmt.Sprintf("**Include:** %s", params.Include))
			}
			if params.LiteralText {
				parts = append(parts, "**Literal:** true")
			}
			return strings.Join(parts, "\n")
		}
	case tools.GlobToolName:
		var params tools.GlobParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**Pattern:** %s", params.Pattern))
			if params.Path != "" {
				parts = append(parts, fmt.Sprintf("**Path:** %s", params.Path))
			}
			return strings.Join(parts, "\n")
		}
	case tools.LSToolName:
		var params tools.LSParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			path := params.Path
			if path == "" {
				path = "."
			}
			return fmt.Sprintf("**Path:** %s", fsext.PrettyPath(path))
		}
	case tools.DownloadToolName:
		var params tools.DownloadParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			var parts []string
			parts = append(parts, fmt.Sprintf("**URL:** %s", params.URL))
			parts = append(parts, fmt.Sprintf("**File Path:** %s", fsext.PrettyPath(params.FilePath)))
			if params.Timeout > 0 {
				parts = append(parts, fmt.Sprintf("**Timeout:** %s", (time.Duration(params.Timeout)*time.Second).String()))
			}
			return strings.Join(parts, "\n")
		}
	case tools.DiagnosticsToolName:
		return "**Project:** diagnostics"
	case agent.AgentToolName:
		var params agent.AgentParams
		if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
			return fmt.Sprintf("**Task:**\n%s", params.Prompt)
		}
	}

	var params map[string]any
	if json.Unmarshal([]byte(t.toolCall.Input), &params) == nil {
		var parts []string
		for key, value := range params {
			displayKey := strings.ReplaceAll(key, "_", " ")
			if len(displayKey) > 0 {
				displayKey = strings.ToUpper(displayKey[:1]) + displayKey[1:]
			}
			parts = append(parts, fmt.Sprintf("**%s:** %v", displayKey, value))
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

// formatResultForCopy formats tool results for clipboard copying.
func (t *baseToolMessageItem) formatResultForCopy() string {
	if t.result == nil {
		return ""
	}

	if t.result.Data != "" {
		if strings.HasPrefix(t.result.MIMEType, "image/") {
			return fmt.Sprintf("[Image: %s]", t.result.MIMEType)
		}
		return fmt.Sprintf("[Media: %s]", t.result.MIMEType)
	}

	switch t.toolCall.Name {
	case tools.BashToolName:
		return t.formatBashResultForCopy()
	case tools.ViewToolName:
		return t.formatViewResultForCopy()
	case tools.EditToolName:
		return t.formatEditResultForCopy()
	case tools.MultiEditToolName:
		return t.formatMultiEditResultForCopy()
	case tools.WriteToolName:
		return t.formatWriteResultForCopy()
	case tools.FetchToolName:
		return t.formatFetchResultForCopy()
	case tools.AgenticFetchToolName:
		return t.formatAgenticFetchResultForCopy()
	case tools.WebFetchToolName:
		return t.formatWebFetchResultForCopy()
	case agent.AgentToolName:
		return t.formatAgentResultForCopy()
	case tools.DownloadToolName, tools.GrepToolName, tools.RipgrepToolName, tools.GlobToolName, tools.LSToolName, tools.DiagnosticsToolName, tools.TodosToolName:
		return fmt.Sprintf("```\n%s\n```", t.result.Content)
	default:
		return t.result.Content
	}
}

// formatBashResultForCopy formats bash tool results for clipboard.
func (t *baseToolMessageItem) formatBashResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var meta tools.BashResponseMetadata
	if t.result.Metadata != "" {
		if err := json.Unmarshal([]byte(t.result.Metadata), &meta); err != nil {
			return t.result.Content
		}
	}

	output := meta.Output
	if output == "" && t.result.Content != tools.BashNoOutput {
		output = t.result.Content
	}

	if output == "" {
		return ""
	}

	return fmt.Sprintf("```bash\n%s\n```", output)
}

// formatViewResultForCopy formats view tool results for clipboard.
func (t *baseToolMessageItem) formatViewResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var meta tools.ViewResponseMetadata
	if t.result.Metadata != "" {
		if err := json.Unmarshal([]byte(t.result.Metadata), &meta); err != nil {
			return t.result.Content
		}
	}

	if meta.Content == "" {
		return t.result.Content
	}

	lang := ""
	if meta.FilePath != "" {
		ext := strings.ToLower(filepath.Ext(meta.FilePath))
		switch ext {
		case ".go":
			lang = "go"
		case ".js", ".mjs":
			lang = "javascript"
		case ".ts":
			lang = "typescript"
		case ".py":
			lang = "python"
		case ".rs":
			lang = "rust"
		case ".java":
			lang = "java"
		case ".c":
			lang = "c"
		case ".cpp", ".cc", ".cxx":
			lang = "cpp"
		case ".sh", ".bash":
			lang = "bash"
		case ".json":
			lang = "json"
		case ".yaml", ".yml":
			lang = "yaml"
		case ".xml":
			lang = "xml"
		case ".html":
			lang = "html"
		case ".css":
			lang = "css"
		case ".md":
			lang = "markdown"
		}
	}

	var result strings.Builder
	if lang != "" {
		fmt.Fprintf(&result, "```%s\n", lang)
	} else {
		result.WriteString("```\n")
	}
	result.WriteString(meta.Content)
	result.WriteString("\n```")

	return result.String()
}

// formatEditResultForCopy formats edit tool results for clipboard.
func (t *baseToolMessageItem) formatEditResultForCopy() string {
	if t.result == nil || t.result.Metadata == "" {
		if t.result != nil {
			return t.result.Content
		}
		return ""
	}

	var meta tools.EditResponseMetadata
	if json.Unmarshal([]byte(t.result.Metadata), &meta) != nil {
		return t.result.Content
	}

	var params tools.EditParams
	if err := json.Unmarshal([]byte(t.toolCall.Input), &params); err != nil {
		// Malformed input JSON is non-fatal here; the diff header just omits the pretty file name.
		params = tools.EditParams{}
	}

	var result strings.Builder

	if meta.OldContent != "" || meta.NewContent != "" {
		fileName := params.FilePath
		if fileName != "" {
			fileName = fsext.PrettyPath(fileName)
		}
		diffContent, additions, removals := diff.GenerateDiff(meta.OldContent, meta.NewContent, fileName)

		fmt.Fprintf(&result, "Changes: +%d -%d\n", additions, removals)
		result.WriteString("```diff\n")
		result.WriteString(diffContent)
		result.WriteString("\n```")
	}

	return result.String()
}

// formatMultiEditResultForCopy formats multi-edit tool results for clipboard.
func (t *baseToolMessageItem) formatMultiEditResultForCopy() string {
	if t.result == nil || t.result.Metadata == "" {
		if t.result != nil {
			return t.result.Content
		}
		return ""
	}

	var meta tools.MultiEditResponseMetadata
	if json.Unmarshal([]byte(t.result.Metadata), &meta) != nil {
		return t.result.Content
	}

	var params tools.MultiEditParams
	if err := json.Unmarshal([]byte(t.toolCall.Input), &params); err != nil {
		// Malformed input JSON is non-fatal here; the diff header just omits the pretty file name.
		params = tools.MultiEditParams{}
	}

	var result strings.Builder
	if meta.OldContent != "" || meta.NewContent != "" {
		fileName := params.FilePath
		if fileName != "" {
			fileName = fsext.PrettyPath(fileName)
		}
		diffContent, additions, removals := diff.GenerateDiff(meta.OldContent, meta.NewContent, fileName)

		fmt.Fprintf(&result, "Changes: +%d -%d\n", additions, removals)
		result.WriteString("```diff\n")
		result.WriteString(diffContent)
		result.WriteString("\n```")
	}

	return result.String()
}

// formatWriteResultForCopy formats write tool results for clipboard.
func (t *baseToolMessageItem) formatWriteResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var params tools.WriteParams
	if json.Unmarshal([]byte(t.toolCall.Input), &params) != nil {
		return t.result.Content
	}

	lang := ""
	if params.FilePath != "" {
		ext := strings.ToLower(filepath.Ext(params.FilePath))
		switch ext {
		case ".go":
			lang = "go"
		case ".js", ".mjs":
			lang = "javascript"
		case ".ts":
			lang = "typescript"
		case ".py":
			lang = "python"
		case ".rs":
			lang = "rust"
		case ".java":
			lang = "java"
		case ".c":
			lang = "c"
		case ".cpp", ".cc", ".cxx":
			lang = "cpp"
		case ".sh", ".bash":
			lang = "bash"
		case ".json":
			lang = "json"
		case ".yaml", ".yml":
			lang = "yaml"
		case ".xml":
			lang = "xml"
		case ".html":
			lang = "html"
		case ".css":
			lang = "css"
		case ".md":
			lang = "markdown"
		}
	}

	var result strings.Builder
	fmt.Fprintf(&result, "File: %s\n", fsext.PrettyPath(params.FilePath))
	if lang != "" {
		fmt.Fprintf(&result, "```%s\n", lang)
	} else {
		result.WriteString("```\n")
	}
	result.WriteString(params.Content)
	result.WriteString("\n```")

	return result.String()
}

// formatFetchResultForCopy formats fetch tool results for clipboard.
func (t *baseToolMessageItem) formatFetchResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var params tools.FetchParams
	if json.Unmarshal([]byte(t.toolCall.Input), &params) != nil {
		return t.result.Content
	}

	var result strings.Builder
	if params.URL != "" {
		fmt.Fprintf(&result, "URL: %s\n", params.URL)
	}
	if params.Format != "" {
		fmt.Fprintf(&result, "Format: %s\n", params.Format)
	}
	if params.Timeout > 0 {
		fmt.Fprintf(&result, "Timeout: %ds\n", params.Timeout)
	}
	result.WriteString("\n")

	result.WriteString(t.result.Content)

	return result.String()
}

// formatAgenticFetchResultForCopy formats agentic fetch tool results for clipboard.
func (t *baseToolMessageItem) formatAgenticFetchResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var params tools.AgenticFetchParams
	if json.Unmarshal([]byte(t.toolCall.Input), &params) != nil {
		return t.result.Content
	}

	var result strings.Builder
	if params.URL != "" {
		fmt.Fprintf(&result, "URL: %s\n", params.URL)
	}
	if params.Prompt != "" {
		fmt.Fprintf(&result, "Prompt: %s\n\n", params.Prompt)
	}

	result.WriteString("```markdown\n")
	result.WriteString(t.result.Content)
	result.WriteString("\n```")

	return result.String()
}

// formatWebFetchResultForCopy formats web fetch tool results for clipboard.
func (t *baseToolMessageItem) formatWebFetchResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var params tools.WebFetchParams
	if json.Unmarshal([]byte(t.toolCall.Input), &params) != nil {
		return t.result.Content
	}

	var result strings.Builder
	fmt.Fprintf(&result, "URL: %s\n\n", params.URL)
	result.WriteString("```markdown\n")
	result.WriteString(t.result.Content)
	result.WriteString("\n```")

	return result.String()
}

// formatAgentResultForCopy formats agent tool results for clipboard.
func (t *baseToolMessageItem) formatAgentResultForCopy() string {
	if t.result == nil {
		return ""
	}

	var result strings.Builder

	if t.result.Content != "" {
		fmt.Fprintf(&result, "```markdown\n%s\n```", t.result.Content)
	}

	return result.String()
}

// prettifyToolName returns a human-readable name for tool names.
func prettifyToolName(name string) string {
	switch name {
	case agent.AgentToolName:
		return "Agent"
	case tools.BashToolName:
		return "Bash"
	case tools.JobOutputToolName:
		return "Job: Output"
	case tools.JobKillToolName:
		return "Job: Kill"
	case tools.DownloadToolName:
		return "Download"
	case tools.EditToolName:
		return "Edit"
	case tools.MultiEditToolName:
		return "Multi-Edit"
	case tools.FetchToolName:
		return "Fetch"
	case tools.AgenticFetchToolName:
		return "Agentic Fetch"
	case tools.WebFetchToolName:
		return "Fetch"
	case tools.WebSearchToolName:
		return "Search"
	case tools.GlobToolName:
		return "Glob"
	case tools.GrepToolName:
		return "Grep"
	case tools.RipgrepToolName:
		return "Ripgrep"
	case tools.LSToolName:
		return "List"
	case tools.TodosToolName:
		return "To-Do"
	case tools.ViewToolName:
		return "Read"
	case tools.WriteToolName:
		return "Write"
	default:
		return humanizedToolName(name)
	}
}
