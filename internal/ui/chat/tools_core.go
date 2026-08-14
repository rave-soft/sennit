package chat

import (
	"strings"
	"time"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/message"
	tools "github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/ui/anim"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// responseContextHeight limits the number of lines displayed in tool output.
// Regular tool calls (view/write/edit/bash/grep/...) never show a body at
// all — see appendResultSummary — so in practice this only still bounds the
// still-alive running-delegation preview in tools.go (toolOutputMarkdownContent).
const responseContextHeight = 10

// previewTruncateFormat notes how much of a body was cut off. The tools
// rendering through this (the running-delegation preview in tools.go) have
// no click-to-see-more, so unlike assistantMessageTruncateFormat (the
// assistant's own message text) and Bash's click-to-expand body, this
// never invites a click.
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
	// Expanded reports the item's click-to-expand state for renderers
	// that show a collapsible body (currently only Bash).
	Expanded bool
	// Hovered reports that the pointer is over the expandable tool item.
	Hovered bool
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
	// expanded is the click-to-expand state, consumed by renderers that
	// show a collapsible body (see ToolRenderOpts.Expanded). Toggled via
	// Expandable on the concrete item types that opt in (e.g. Bash).
	expanded bool
	// hovered reports that the pointer is over the expand/collapse hint.
	hovered bool
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

var toolMessageItemFactories = map[string]ToolRenderer{
	tools.BashToolName:          &BashToolRenderContext{},
	tools.JobOutputToolName:     &JobOutputToolRenderContext{},
	tools.JobKillToolName:       &JobKillToolRenderContext{},
	tools.ViewToolName:          &ViewToolRenderContext{},
	tools.WriteToolName:         &WriteToolRenderContext{},
	tools.EditToolName:          &EditToolRenderContext{},
	tools.MultiEditToolName:     &MultiEditToolRenderContext{},
	tools.GlobToolName:          &GlobToolRenderContext{},
	tools.GrepToolName:          &GrepToolRenderContext{title: "Grep"},
	tools.RipgrepToolName:       &GrepToolRenderContext{title: "Ripgrep"},
	tools.LSToolName:            &LSToolRenderContext{},
	tools.DownloadToolName:      &DownloadToolRenderContext{},
	tools.FetchToolName:         &FetchToolRenderContext{},
	tools.AgenticFetchToolName:  &AgenticFetchToolRenderContext{},
	tools.DiagnosticsToolName:   &DiagnosticsToolRenderContext{},
	tools.WebFetchToolName:      &WebFetchToolRenderContext{},
	tools.WebSearchToolName:     &WebSearchToolRenderContext{},
	tools.TodosToolName:         &TodosToolRenderContext{},
	tools.QuestionToolName:      &QuestionToolRenderContext{},
	tools.ReferencesToolName:    &ReferencesToolRenderContext{},
	tools.DefinitionToolName:    &DefinitionToolRenderContext{},
	tools.RenameToolName:        &RenameToolRenderContext{},
	tools.ReplaceSymbolToolName: &ReplaceSymbolToolRenderContext{},
	tools.CallHierarchyToolName: &CallHierarchyToolRenderContext{},
	tools.SymbolsToolName:       &SymbolsToolRenderContext{},
	tools.LSPRestartToolName:    &LSPRestartToolRenderContext{},
}

func newRegisteredToolMessageItem(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, renderer ToolRenderer, canceled bool) ToolMessageItem {
	switch renderer.(type) {
	case *BashToolRenderContext:
		return &BashToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)}
	case *AgenticFetchToolRenderContext:
		item := &AgenticFetchToolMessageItem{startTime: time.Now()}
		item.baseToolMessageItem = newBaseToolMessageItem(sty, toolCall, result, &AgenticFetchToolRenderContext{fetch: item}, canceled)
		item.spinningFunc = func(state SpinningState) bool {
			return !state.HasResult() && !state.IsCanceled()
		}
		return item
	case *WriteToolRenderContext:
		return &WriteToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)}
	case *EditToolRenderContext:
		return &EditToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)}
	case *MultiEditToolRenderContext:
		return &MultiEditToolMessageItem{newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)}
	default:
		return newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)
	}
}

// NewToolMessageItem creates a new [ToolMessageItem] based on the tool call name.
//
// It returns a specific tool message item type if implemented, otherwise it
// returns a generic tool message item. The messageID is the ID of the assistant
// message containing this tool call. cfg is used to recognize user-defined
// agent tools (see isCustomAgentTool) so they get the same renderer as the
// built-in "agent" tool; it may be nil, in which case no tool name is
// treated as a custom tools.
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
	case toolCall.Name == tools.AgentToolName:
		item = NewAgentToolMessageItem(sty, toolCall, result, canceled, cfg)
	case toolMessageItemFactories[toolCall.Name] != nil:
		item = newRegisteredToolMessageItem(sty, toolCall, result, toolMessageItemFactories[toolCall.Name], canceled)
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
// domain/agent/custom_agent_tool.go registers one delegation tool per
// entry in cfg.Agents, named after the agent's id — excluding "coder" and
// "task", which are the built-in roles rather than tools a model can call.
func isCustomAgentTool(cfg *config.Config, name string) bool {
	if cfg == nil || name == config.AgentCoder || name == config.AgentTask {
		return false
	}
	_, ok := cfg.Agents[name]
	return ok
}
