package chat

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/message"
	tools "github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/ui/anim"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
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

// IsPending returns true if the tool call is still pending (not finished, no
// result yet, and not canceled). Renderers turn this into the pending spinner
// block, so it has to agree with baseToolMessageItem.isSpinning — including on
// the result: a call whose result landed while Finished stayed false would
// otherwise render a spinner that no longer receives animation ticks, which
// reads as a frozen one rather than an absent one.
func (o *ToolRenderOpts) IsPending() bool {
	return !o.ToolCall.Finished && !o.HasResult() && !o.IsCanceled()
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
	t.anim = t.newAnim()

	return t
}

// newAnim builds the pending-tool animation from the item's current styles.
// Construction and [baseToolMessageItem.Restyle] share it so a rebuilt
// animation cannot drift from the original settings.
func (t *baseToolMessageItem) newAnim() *anim.Anim {
	return anim.New(anim.Settings{
		ID:          t.toolCall.ID,
		Size:        15,
		GradColorA:  t.sty.WorkingGradFromColor,
		GradColorB:  t.sty.WorkingGradToColor,
		LabelColor:  t.sty.WorkingLabelColor,
		CycleColors: true,
	})
}

// Restyle implements [Restylable]. See [AssistantMessageItem.Restyle].
func (t *baseToolMessageItem) Restyle() tea.Cmd {
	t.anim = t.newAnim()
	t.Bump()
	return t.StartAnimation()
}

func newRegisteredToolMessageItem(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, renderer ToolRenderer, canceled bool) ToolMessageItem {
	if factory, ok := toolItemFactories[toolCall.Name]; ok {
		return factory(sty, toolCall, result, canceled)
	}
	return newBaseToolMessageItem(sty, toolCall, result, renderer, canceled)
}

// CustomAgentConfig is the narrow config capability tool renderers need to
// recognize a user-defined agent tool and read its model/effort override —
// nothing more, so this package never has to import internal/config or name
// its Agent type. *config.Config already implements this via AgentOverride,
// which excludes the built-in "coder"/"task" roles this package treats
// specially.
type CustomAgentConfig interface {
	// AgentOverride reports whether name is a user-defined agent tool and,
	// if so, its configured model/reasoning-effort override.
	AgentOverride(name string) (model, effort string, ok bool)
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
	cfg CustomAgentConfig,
) ToolMessageItem {
	var item ToolMessageItem
	switch {
	case toolCall.Name == tools.AgentToolName:
		item = NewAgentToolMessageItem(sty, toolCall, result, canceled, cfg)
	case IsDockerMCPTool(toolCall.Name):
		item = NewDockerMCPToolMessageItem(sty, toolCall, result, canceled)
	case strings.HasPrefix(toolCall.Name, "mcp_"):
		item = NewMCPToolMessageItem(sty, toolCall, result, canceled)
	case isCustomAgentTool(cfg, toolCall.Name):
		// User-defined agents (.sennit/agents, config.Agents) are
		// delegations of the same shape as the built-in "agent" tool —
		// CustomAgentParams mirrors AgentParams, just a "prompt" field —
		// so they get the identical renderer: running status line,
		// collapse-to-summary once finished, click-to-drill into the
		// child session.
		item = NewAgentToolMessageItem(sty, toolCall, result, canceled, cfg)
	default:
		if renderer, _, ok := lookupToolRenderer(toolCall.Name); ok {
			item = newRegisteredToolMessageItem(sty, toolCall, result, renderer, canceled)
		} else {
			item = NewGenericToolMessageItem(sty, toolCall, result, canceled)
		}
	}
	item.SetMessageID(messageID)
	return item
}

// isCustomAgentTool reports whether name is a user-defined agent tool.
// domain/agent/custom_agent_tool.go registers one delegation tool per
// entry in cfg.Agents, named after the agent's id — AgentOverride already
// excludes "coder" and "task", the built-in roles rather than tools a
// model can call.
func isCustomAgentTool(cfg CustomAgentConfig, name string) bool {
	if cfg == nil {
		return false
	}
	_, _, ok := cfg.AgentOverride(name)
	return ok
}
