package chat

import (
	"fmt"
	"image"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"charm.land/lipgloss/v2"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/attachments"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// MessageLeftPaddingTotal is the total width taken up by the border and
// padding.
const MessageLeftPaddingTotal = 2

// maxTextWidth is the maximum width text messages can be.
const maxTextWidth = 120

// Identifiable is an interface for items that can provide a unique identifier.
type Identifiable interface {
	ID() string
}

// Animatable is an interface for items that support animation.
type Animatable interface {
	StartAnimation() tea.Cmd
	Animate(msg spin.StepMsg) tea.Cmd
}

// Expandable is an interface for items that can be expanded or collapsed.
type Expandable interface {
	// ToggleExpanded toggles the expanded state of the item. It returns
	// whether the item is now expanded.
	ToggleExpanded() bool
}

// Hoverable lets the chat provide mouse hover feedback for clickable items.
type Hoverable interface {
	HoverableAt(x, y, width int) bool
	SetHovered(bool)
}

// KeyEventHandler is an interface for items that can handle key events.
type KeyEventHandler interface {
	HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd)
}

// MessageItem represents a [message.Message] item that can be displayed in the
// UI and be part of a [list.List] identifiable by a unique ID.
type MessageItem interface {
	list.Item
	list.RawRenderable
	Identifiable
}

// HighlightableMessageItem is a message item that supports highlighting.
type HighlightableMessageItem interface {
	MessageItem
	list.Highlightable
}

// FocusableMessageItem is a message item that supports focus.
type FocusableMessageItem interface {
	MessageItem
	list.Focusable
}

// SendMsg represents a message to send a chat message.
type SendMsg struct {
	Text        string
	Attachments []message.Attachment
}

type highlightableMessageItem struct {
	// version is the parent item's version counter. SetHighlight
	// bumps it on every observable change so the F6 list memo and
	// any frozen entry get invalidated when a selection drag enters
	// or leaves the item.
	version *list.Versioned

	startLine   int
	startCol    int
	endLine     int
	endCol      int
	highlighter list.Highlighter
}

var _ list.Highlightable = (*highlightableMessageItem)(nil)

// isHighlighted returns true if the item has a highlight range set.
func (h *highlightableMessageItem) isHighlighted() bool {
	return h.startLine != -1 || h.endLine != -1
}

// renderHighlighted highlights the content if necessary.
func (h *highlightableMessageItem) renderHighlighted(content string, width, height int) string {
	if !h.isHighlighted() {
		return content
	}
	area := image.Rect(0, 0, width, height)
	return list.Highlight(content, area, h.startLine, h.startCol, h.endLine, h.endCol, h.highlighter)
}

// SetHighlight implements list.Highlightable.
func (h *highlightableMessageItem) SetHighlight(startLine int, startCol int, endLine int, endCol int) {
	// Adjust columns for the style's left inset (border + padding) since we
	// highlight the content only.
	offset := MessageLeftPaddingTotal
	newStartCol := max(0, startCol-offset)
	newEndCol := endCol
	if endCol >= 0 {
		newEndCol = max(0, endCol-offset)
	}
	if h.startLine == startLine && h.startCol == newStartCol && h.endLine == endLine && h.endCol == newEndCol {
		return
	}
	h.startLine = startLine
	h.startCol = newStartCol
	h.endLine = endLine
	h.endCol = newEndCol
	if h.version != nil {
		h.version.Bump()
	}
}

// Highlight implements list.Highlightable.
func (h *highlightableMessageItem) Highlight() (startLine int, startCol int, endLine int, endCol int) {
	return h.startLine, h.startCol, h.endLine, h.endCol
}

func defaultHighlighter(sty *styles.Styles, v *list.Versioned) *highlightableMessageItem {
	return &highlightableMessageItem{
		version:     v,
		startLine:   -1,
		startCol:    -1,
		endLine:     -1,
		endCol:      -1,
		highlighter: list.ToHighlighter(sty.TextSelection),
	}
}

// cacheClearable is implemented by message items that cache rendered
// output and can be asked to drop the cache.
type cacheClearable interface {
	clearCache()
}

// ClearItemCaches drops any cached rendered output on each item so the
// next render uses the current styles. It also bumps each item's
// version so the F6 list-level memo invalidates frozen entries on
// the next render.
func ClearItemCaches(items []MessageItem) {
	for _, item := range items {
		if cc, ok := item.(cacheClearable); ok {
			cc.clearCache()
		}
		if v, ok := item.(interface{ Bump() }); ok {
			v.Bump()
		}
	}
}

// Restylable is implemented by message items that hold palette-derived
// state Render cannot re-read: an [spin.Anim] pre-renders its gradient
// frames from the colors it was built with, so a running spinner keeps the
// old palette until the anim itself is rebuilt. Restyle rebuilds it and
// returns the command that re-arms the tick chain, if the item was
// animating (rebuilding resets the anim's generation, which drops the
// in-flight ticks).
type Restylable interface {
	Restyle() tea.Cmd
}

// RestyleItems rebuilds the palette-derived state of every item that
// implements [Restylable] and batches the commands that resume their
// animations. Call it alongside [ClearItemCaches] after a theme switch.
func RestyleItems(items []MessageItem) tea.Cmd {
	var cmds []tea.Cmd
	for _, item := range items {
		if r, ok := item.(Restylable); ok {
			if cmd := r.Restyle(); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	return tea.Batch(cmds...)
}

// cachedMessageItem caches rendered message content to avoid re-rendering.
// Embed this in any message item that stores a cached render (user,
// assistant, and so on).
//
// THOUGHT(kujtim): consider whether caching the render per width is worth
// the memory it would cost.
type cachedMessageItem struct {
	// rendered is the cached rendered string
	rendered string
	// width and height are the dimensions of the cached render
	width  int
	height int

	// prefixedRendered caches the per-line-prefixed Render output (the
	// result of splitting RawRender by newlines and prepending a focus
	// or selection prefix to every line). Items rebuild this every
	// frame today; caching it keyed by (prefixedWidth, prefixedKey)
	// turns Render into a pointer return when item state is stable.
	//
	// Invalidation lives in clearCache; callers must additionally
	// bypass this cache whenever the prefixed output would not be
	// stable (spinner ticks, active highlight ranges) by not calling
	// setCachedPrefixedRender for those frames.
	prefixedRendered string
	prefixedWidth    int
	prefixedKey      uint64
}

// getCachedRender returns the cached render if it exists for the given width.
func (c *cachedMessageItem) getCachedRender(width int) (string, int, bool) {
	if c.width == width && c.rendered != "" {
		return c.rendered, c.height, true
	}
	return "", 0, false
}

// setCachedRender sets the cached render.
func (c *cachedMessageItem) setCachedRender(rendered string, width, height int) {
	c.rendered = rendered
	c.width = width
	c.height = height
}

// getCachedPrefixedRender returns the cached prefixed render if it exists
// for the given (width, key). The key encodes any state that changes the
// per-line prefix (focused/blurred, compact, ...).
func (c *cachedMessageItem) getCachedPrefixedRender(width int, key uint64) (string, bool) {
	if c.prefixedRendered != "" && c.prefixedWidth == width && c.prefixedKey == key {
		return c.prefixedRendered, true
	}
	return "", false
}

// setCachedPrefixedRender stores the cached prefixed render.
func (c *cachedMessageItem) setCachedPrefixedRender(rendered string, width int, key uint64) {
	c.prefixedRendered = rendered
	c.prefixedWidth = width
	c.prefixedKey = key
}

// prefixLines splits content on newlines and prepends prefix to every
// line before rejoining. Every MessageItem's Render builds its output
// this way: RawRender already wraps content to width, so restyling it
// line-by-line avoids handing the whole block to lipgloss.Render, whose
// wrapping pass gets expensive on long messages (see the comment on
// AssistantMessageItem.Render).
func prefixLines(content, prefix string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// renderCachedPrefixed implements the "check the prefixed-render cache,
// else build and store" shape shared by every MessageItem.Render that
// caches its output. build computes the fully prefixed content for width;
// it is only called on a cache miss. useCache lets a caller bypass the
// cache for a frame whose content the key doesn't fully capture (a
// spinner tick, an active highlight range) without having to duplicate
// the get/set bracketing at each call site.
func (c *cachedMessageItem) renderCachedPrefixed(width int, key uint64, useCache bool, build func() string) string {
	if useCache {
		if cached, ok := c.getCachedPrefixedRender(width, key); ok {
			return cached
		}
	}
	out := build()
	if useCache {
		c.setCachedPrefixedRender(out, width, key)
	}
	return out
}

// clearCache clears the cached render.
func (c *cachedMessageItem) clearCache() {
	c.rendered = ""
	c.width = 0
	c.height = 0
	c.prefixedRendered = ""
	c.prefixedWidth = 0
	c.prefixedKey = 0
}

// focusableMessageItem is a base struct for message items that can be focused.
type focusableMessageItem struct {
	// version is the parent item's version counter. SetFocused
	// bumps it whenever focus actually flips so the F6 list memo
	// invalidates the per-line focus prefix.
	version *list.Versioned
	focused bool
}

// newFocusableMessageItem returns a focusableMessageItem wired to the
// shared version counter.
func newFocusableMessageItem(v *list.Versioned) *focusableMessageItem {
	return &focusableMessageItem{version: v}
}

// SetFocused implements MessageItem.
func (f *focusableMessageItem) SetFocused(focused bool) {
	if f.focused == focused {
		return
	}
	f.focused = focused
	if f.version != nil {
		f.version.Bump()
	}
}

// AssistantInfoID returns a stable ID for assistant info items.
func AssistantInfoID(messageID string) string {
	return fmt.Sprintf("%s:assistant-info", messageID)
}

// ModelConfig is the narrow slice of configuration AssistantInfoItem needs
// to render its "via <provider> using <model>" footer: resolving a model's
// catalog metadata and a provider's display name. *config.Config already
// implements this, so callers keep passing it as-is; the interface just
// keeps this package from having to import internal/config's full store
// type into a renderer signature.
type ModelConfig interface {
	GetModel(provider, model string) *catwalk.Model
	ProviderName(provider string) (name string, ok bool)
}

// AssistantInfoItem renders model info and response time after assistant completes.
type AssistantInfoItem struct {
	*list.Versioned
	*cachedMessageItem

	id                  string
	message             *message.Message
	sty                 *styles.Styles
	cfg                 ModelConfig
	lastUserMessageTime time.Time
}

// NewAssistantInfoItem creates a new AssistantInfoItem.
func NewAssistantInfoItem(sty *styles.Styles, message *message.Message, cfg ModelConfig, lastUserMessageTime time.Time) MessageItem {
	return &AssistantInfoItem{
		Versioned:           list.NewVersioned(),
		cachedMessageItem:   &cachedMessageItem{},
		id:                  AssistantInfoID(message.ID),
		message:             message,
		sty:                 sty,
		cfg:                 cfg,
		lastUserMessageTime: lastUserMessageTime,
	}
}

// Finished implements list.Item. Assistant info blocks render a fixed
// model/duration footer once the assistant turn finishes; the data
// is immutable after construction so the entry is safe to freeze.
func (a *AssistantInfoItem) Finished() bool {
	return true
}

// ID implements MessageItem.
func (a *AssistantInfoItem) ID() string {
	return a.id
}

// RawRender implements MessageItem.
func (a *AssistantInfoItem) RawRender(width int) string {
	innerWidth := max(0, width-MessageLeftPaddingTotal)
	content, _, ok := a.getCachedRender(innerWidth)
	if !ok {
		content = a.renderContent(innerWidth)
		height := lipgloss.Height(content)
		a.setCachedRender(content, innerWidth, height)
	}
	return content
}

// Render implements MessageItem.
func (a *AssistantInfoItem) Render(width int) string {
	// AssistantInfoItem uses a single, state-independent prefix; key 0
	// is sufficient. The cache is invalidated whenever the underlying
	// cachedMessageItem render is cleared.
	return a.renderCachedPrefixed(width, 0, true, func() string {
		prefix := a.sty.Messages.SectionHeader.Render()
		return prefixLines(a.RawRender(width), prefix)
	})
}

func (a *AssistantInfoItem) renderContent(width int) string {
	finishData := a.message.FinishPart()
	if finishData == nil {
		return ""
	}
	finishTime := time.Unix(finishData.Time, 0)
	duration := finishTime.Sub(a.lastUserMessageTime)
	infoMsg := a.sty.Messages.AssistantInfoDuration.Render(fmt.Sprintf("in %s", duration))
	icon := a.sty.Messages.AssistantInfoIcon.Render(styles.ModelIcon)
	model := a.cfg.GetModel(a.message.Provider, a.message.Model)
	if model == nil {
		model = &catwalk.Model{Name: "Unknown Model"}
	}
	modelFormatted := a.sty.Messages.AssistantInfoModel.Render(model.Name)
	providerName := a.message.Provider
	if name, ok := a.cfg.ProviderName(a.message.Provider); ok {
		providerName = name
	}
	provider := a.sty.Messages.AssistantInfoProvider.Render(fmt.Sprintf("via %s", providerName))
	assistant := fmt.Sprintf("%s %s %s %s", icon, modelFormatted, provider, infoMsg)
	return common.Section(a.sty, assistant, width)
}

// cappedMessageWidth returns the maximum width for message content for readability.
func cappedMessageWidth(availableWidth int) int {
	return min(availableWidth-MessageLeftPaddingTotal, maxTextWidth)
}

// ExtractMessageItems extracts [MessageItem]s from a [message.Message]. It
// returns all parts of the message as [MessageItem]s.
//
// For assistant messages with tool calls, pass a toolResults map to link
// results. Use BuildToolResultMap to create this map from all messages in
// a session. cfg is forwarded to NewToolMessageItem to recognize
// user-defined agent tools; it may be nil.
func ExtractMessageItems(sty *styles.Styles, msg *message.Message, toolResults map[string]ToolResultRef, cfg CustomAgentConfig) []MessageItem {
	switch msg.Role {
	case message.User:
		// Reconstruct shell command items from ShellCommand parts.
		var items []MessageItem
		for _, part := range msg.Parts {
			if sc, ok := part.(message.ShellCommand); ok {
				items = append(items, NewShellItem(sty, sc.Command, sc.Output, sc.ExitCode))
			}
		}
		if len(items) > 0 {
			return items
		}
		r := attachments.NewRenderer(
			sty.Attachments.Normal,
			sty.Attachments.Deleting,
			sty.Attachments.Image,
			sty.Attachments.Text,
			sty.Attachments.Skill,
			sty.Attachments.Remove,
		)
		return []MessageItem{NewUserMessageItem(sty, msg, r)}
	case message.Assistant:
		var items []MessageItem
		if ShouldRenderAssistantMessage(msg) {
			items = append(items, NewAssistantMessageItem(sty, msg))
		}
		for _, tc := range msg.ToolCalls() {
			var result *message.ToolResult
			var finishedAt int64
			if ref, ok := toolResults[tc.ID]; ok {
				result = &ref.Result
				finishedAt = ref.CreatedAt
			}
			item := NewToolMessageItem(
				sty,
				msg.ID,
				tc,
				result,
				msg.FinishReason() == message.FinishReasonCanceled,
				cfg,
			)
			// A delegation rebuilt here ran between the two stored
			// messages, not from now on — see RestoreTiming. Timestamps
			// are skipped when absent, which would otherwise date the
			// delegation to 1970 and report decades of elapsed time.
			if r, ok := item.(DelegationTimingRestorer); ok {
				r.RestoreTiming(unixOrZero(msg.CreatedAt), unixOrZero(finishedAt))
			}
			items = append(items, item)
		}
		return items
	case message.System:
		// A record of something Sennit did, not something either party
		// said. See NoticeItem.
		if strings.TrimSpace(msg.Content().Text) == "" {
			return nil
		}
		return []MessageItem{NewNoticeItem(sty, msg)}
	}
	return []MessageItem{}
}

// unixOrZero converts a stored second-granularity timestamp to a
// time.Time, mapping the "not recorded" zero to the zero time rather than
// to the Unix epoch.
func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// ShouldRenderAssistantMessage reports whether an assistant message should
// be rendered.
//
// An assistant message that only carries tool calls has no text of its
// own, so it must not render as an empty message.
func ShouldRenderAssistantMessage(msg *message.Message) bool {
	content := strings.TrimSpace(msg.Content().Text)
	thinking := strings.TrimSpace(msg.ReasoningContent().Thinking)
	isCancelled := msg.FinishReason() == message.FinishReasonCanceled
	hasToolCalls := len(msg.ToolCalls()) > 0
	return !hasToolCalls || content != "" || thinking != "" || msg.IsThinking() || msg.IsErrorLike() || isCancelled
}

// ToolResultRef is a tool result together with the timestamp of the tool
// message that carried it. The timestamp is when the tool call finished,
// which is what lets a delegation rebuilt from history report the runtime
// it actually had instead of an unknown one (see RestoreTiming).
type ToolResultRef struct {
	Result    message.ToolResult
	CreatedAt int64
}

// BuildToolResultMap creates a map of tool call IDs to their results from
// a list of messages. Tool result messages (role == message.Tool) carry
// the results that get linked to tool calls in assistant messages.
func BuildToolResultMap(messages []*message.Message) map[string]ToolResultRef {
	resultMap := make(map[string]ToolResultRef)
	for _, msg := range messages {
		if msg.Role == message.Tool {
			for _, result := range msg.ToolResults() {
				if result.ToolCallID != "" {
					resultMap[result.ToolCallID] = ToolResultRef{Result: result, CreatedAt: msg.CreatedAt}
				}
			}
		}
	}
	return resultMap
}
