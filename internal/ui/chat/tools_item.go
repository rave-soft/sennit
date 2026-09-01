package chat

import (
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/spin"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// SetCompact implements the Compactable interface.
func (t *baseToolMessageItem) SetCompact(compact bool) {
	if t.isCompact == compact {
		return
	}
	t.isCompact = compact
	t.clearCache()
	t.Bump()
}

// Hidden implements [list.Hideable].
func (t *baseToolMessageItem) Hidden() bool { return t.hiddenWhilePanelled }

// SetHiddenWhilePanelled records whether the session panel is currently
// showing what this row would say, and is the only thing that can make a
// tool item draw nothing. See the hiddenWhilePanelled field.
func (t *baseToolMessageItem) SetHiddenWhilePanelled(hidden bool) {
	if t.hiddenWhilePanelled == hidden {
		return
	}
	t.hiddenWhilePanelled = hidden
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
func (t *baseToolMessageItem) Animate(msg spin.StepMsg) tea.Cmd {
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
//
// This is the one place a tool item's content width is computed: it is
// handed unchanged to RenderTool and to the hook indicator below, so the
// body and the indicator always agree, and individual RenderTool
// implementations must not re-derive or re-cap it. The max(0, ...) guard
// keeps toolItemWidth from going negative on a terminal narrower than
// MessageLeftPaddingTotal, which would otherwise propagate into
// lipgloss.Width() calls downstream.
func (t *baseToolMessageItem) RawRender(width int) string {
	toolItemWidth := max(0, width-MessageLeftPaddingTotal)
	if t.hasCappedWidth {
		toolItemWidth = max(0, cappedMessageWidth(width))
	}

	content, height, ok := t.getCachedRender(toolItemWidth)
	// if we are spinning or there is no cache rerender
	if !ok || t.isSpinning() {
		t.syncAnimLabel()
		content = t.toolRenderer.RenderTool(t.sty, toolItemWidth, &ToolRenderOpts{
			ToolCall: t.toolCall,
			Result:   t.result,
			Anim:     t.anim,
			Compact:  t.isCompact,
			Status:   t.computeStatus(),
			Expanded: t.expanded,
			Hovered:  t.hovered,
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

// syncAnimLabel gives the pending animation a word for what the call is
// doing, so a running tool is not just an unlabeled band of glyphs next
// to its name.
//
// Only the arguments-still-streaming case is labeled. That is the wait a
// reader cannot otherwise account for: the call is known, nothing is
// executing yet, and on a long argument stream (a file written in one go)
// the line would otherwise sit unchanged for minutes. Once the call is
// finished the spinner belongs to a tool that is actually executing, and
// the two kinds that keep spinning there — the delegations — already
// print a status line saying elapsed, step and last nested tool
// underneath; a second, vaguer "Running" beside the name would only
// compete with it.
//
// SetLabel re-renders the label and recomputes the animation width, so it
// runs on a change of wording rather than on every frame.
func (t *baseToolMessageItem) syncAnimLabel() {
	if t.anim == nil {
		return
	}
	var label string
	if !t.toolCall.Finished {
		label = "Preparing"
	}
	if label == t.animLabel {
		return
	}
	t.animLabel = label
	t.anim.SetLabel(label)
}

// Render renders the tool message item at the given width.
func (t *baseToolMessageItem) Render(width int) string {
	// A hidden item draws nothing — not an empty prefixed line, which is
	// what prefixLines would make of an empty body. See [Hideable]. Asked
	// of the item first (the panel handoff sets its flag directly) and
	// then of the renderer, for a renderer that decides on its own.
	if t.Hidden() {
		return ""
	}
	if hideable, ok := t.toolRenderer.(list.Hideable); ok && hideable.Hidden() {
		return ""
	}
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
	return t.renderCachedPrefixed(width, key, useCache, func() string {
		var prefix string
		if t.isCompact {
			prefix = t.sty.Messages.ToolCallCompact.Render()
		} else if t.focused {
			prefix = t.sty.Messages.ToolCallFocused.Render()
		} else {
			prefix = t.sty.Messages.ToolCallBlurred.Render()
		}
		return prefixLines(t.RawRender(width), prefix)
	})
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

// HasResult reports whether the tool call has come back yet. It reads the
// same field computeStatus does, exposed because Status() alone cannot
// answer it: status stays ToolStatusRunning once a result lands, and only
// computeStatus (unexported, render-time) folds the result in. See
// delegationStarted in internal/ui/model, which needs to tell a delegation
// still in flight from one that has finished.
func (t *baseToolMessageItem) HasResult() bool {
	return t.result != nil
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
//
// A recorded result ends the spin whatever the tool call says about itself.
// Finished means the call's arguments finished streaming, and the agent marks
// it on a path that a hard kill can skip (see the cleanup loop in
// internal/agent's runTurn), so a persisted call can carry Finished=false
// forever. Without the result check such an item spun for the rest of the
// session's life while computeStatus already reported it as a success, and
// Finished below never let the list freeze it.
func (t *baseToolMessageItem) isSpinning() bool {
	if t.spinningFunc != nil {
		return t.spinningFunc(SpinningState{
			ToolCall: t.toolCall,
			Result:   t.result,
			Status:   t.status,
		})
	}
	return !t.toolCall.Finished && t.result == nil && t.status != ToolStatusCanceled
}

// Finished implements list.Item. A tool call is freezable once it can no
// longer be in progress: it has been canceled, or a result has been
// recorded. A result on its own is enough, for the same reason isSpinning
// treats it as decisive — toolCall.Finished describes argument streaming
// and a hard kill can leave it false forever, which used to keep such an
// item permanently unfreezable. A later result update bumps the version,
// and the list cache treats a bump as an implicit unfreeze, so freezing
// here cannot pin stale output. A call still executing (arguments in, no
// result yet) stays unfrozen: its renderer keeps updating the running
// state. Tools that override the spinning logic via spinningFunc would
// short-circuit live ticks; we still gate freezing on isSpinning to keep
// the contract conservative.
func (t *baseToolMessageItem) Finished() bool {
	if t.isSpinning() {
		return false
	}
	return t.status == ToolStatusCanceled || t.result != nil
}

// HandleMouseClick implements MouseClickable. A left click is reported as
// handled so a click on an agent/agentic_fetch delegation still drills into
// its child session (see NestedToolContainer in model/chat.go's
// HandleDelayedClick). For item types that additionally implement
// Expandable (e.g. Bash), the click then toggles their body expansion;
// for the rest it is inert.
func (t *baseToolMessageItem) HandleMouseClick(btn ansi.MouseButton, x, y int) bool {
	return btn == ansi.MouseLeft
}

// toggleExpanded flips the click-to-expand state and invalidates the
// render caches. Concrete item types expose it through the Expandable
// interface (see BashToolMessageItem.ToggleExpanded).
func (t *baseToolMessageItem) toggleExpanded() bool {
	t.expanded = !t.expanded
	t.clearCache()
	t.Bump()
	return t.expanded
}

// HoverableAt reports whether the pointer is over the expandable block below
// the tool header.
func (t *baseToolMessageItem) HoverableAt(x, y, width int) bool {
	return x >= MessageLeftPaddingTotal && y > 0 && y < lipgloss.Height(t.Render(width))
}

func clickableItemHover(sty *styles.Styles, content string, width int, hovered bool) string {
	if !hovered {
		return content
	}
	return common.BlockBackground(content, width, sty.Tool.ClickableHoverBg)
}

// SetHovered updates hover feedback for expandable tool renderers.
func (t *baseToolMessageItem) SetHovered(hovered bool) {
	if t.hovered == hovered {
		return
	}
	t.hovered = hovered
	t.clearCache()
	t.Bump()
}

// HandleKeyEvent implements KeyEventHandler.
func (t *baseToolMessageItem) HandleKeyEvent(key tea.KeyMsg) (bool, tea.Cmd) {
	if k := key.String(); k == "c" || k == "y" {
		text := t.formatToolForCopy()
		return true, common.CopyToClipboard(text, "Tool content copied to clipboard")
	}
	return false, nil
}
