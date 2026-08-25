package model

import (
	"image"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/util"
)

// mouseState holds UI-level mouse hover/click bookkeeping used for
// highlight and double-click detection at the UI level (distinct from
// Chat's own mouse-selection state in chat.go, which tracks drag/click
// inside the message list).
//
// Embedded anonymously (by value) on UI so its fields keep promoting
// unchanged (m.lastClickTime, ...); see widgets.go for why.
type mouseState struct {
	lastClickTime time.Time
	hoverX        int
	hoverY        int
}

// updateMouse handles the mouse-driven branches of UI.Update: clicks,
// motion, release, delayed (double-)click resolution, and coalesced wheel
// events. It is called from Update's message-type switch and shares that
// switch's cmds accumulator.
//
// The second return value reports whether a branch below took one of
// Update's early-return paths (return m, tea.Batch(cmds...)): when true,
// the caller must return immediately with the returned cmds, bypassing the
// rest of Update's tail (the focus/placeholder switch, stale-workspace
// refresh, and attachment update) exactly as the original inline case did.
// When false, a branch fell through instead, and the caller must continue
// running that tail with the returned cmds, exactly as falling out of the
// original case body would.
func (m *UI) updateMouse(msg tea.Msg, cmds []tea.Cmd) ([]tea.Cmd, bool) {
	switch msg := msg.(type) {
	case DelayedClickMsg:
		// Handle delayed single-click action (e.g., expansion, or
		// navigating into a clicked child-session delegation). messageID
		// and toolCallID come from the clicked item directly, not
		// m.chat.SelectedNestedToolContainer — a mouse click doesn't move
		// the keyboard-driven selection that reads (see HandleMouseDown).
		if _, openContainer, messageID, toolCallID := m.chat.HandleDelayedClick(msg); openContainer {
			cmds = append(cmds, m.enterChildSession(messageID, toolCallID))
		}
	case tea.MouseClickMsg:
		// Pass mouse events to dialogs first if any are open. Route through
		// handleDialogMsg (not a bare dialog.Update) so a click's resulting
		// Action — e.g. clicking a permissions dialog button — is actually
		// processed the same way a keyboard-driven selection is.
		if m.dialog.HasDialogs() {
			if cmd := m.handleDialogMsg(msg); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return cmds, true
		}

		// Route clicks to inline editors that support mouse interaction.
		if m.activeInline != nil {
			if clickable, ok := m.activeInline.(dialog.MouseClickableEditor); ok {
				if done, handled, cmd := clickable.HandleMouseClick(msg.X, msg.Y); handled {
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
					if done {
						m.activeInline = nil
						m.editor.textarea.Focus()
						m.updateLayoutAndSize()
					}
					return cmds, true
				}
			}
		}

		// A click anywhere on the header while threads are active opens the
		// threads dashboard — the badge rendered there (see header.go's
		// renderHeaderDetails) is the only visible hint threads are running
		// while on the main screen, so it doubles as a button.
		if msg.Button == tea.MouseLeft && m.activeThreadBadgeCount() > 0 && image.Pt(msg.X, msg.Y).In(m.lay.layout.header) {
			cmds = append(cmds, util.CmdHandler(showThreadsDashboardMsg{}))
			return cmds, true
		}

		// A click on the breadcrumb bar's Back button goes up one level —
		// out of a sub-agent session, or out of the thread itself once none
		// are left. Mouse clicks never change m.focus (see uiFocusState) —
		// this is the one dedicated, deliberate transition a click is
		// allowed to trigger.
		if msg.Button == tea.MouseLeft && m.breadcrumbButtonHit(image.Pt(msg.X, msg.Y)) {
			if cmd := m.breadcrumbBack(); cmd != nil {
				cmds = append(cmds, cmd)
			}
			return cmds, true
		}

		// A click on the session panel's todos header row toggles its
		// expand state; a click on a rendered thread block drills into
		// that thread's own session — the same transition Enter takes on
		// the threads dashboard (see Root.attachThreadCmd), not
		// enterChildSession/navStack, which point at the wrong workspace.
		//
		// Hit-test rects are recomputed here from m.lay.layout.panel +
		// m.sessionPanelPlan (via sessionPanelRowLayout), NOT read from
		// m.panel.todosHeaderRect/m.panel.threadRects: those are only
		// populated as a side effect of drawSessionPanel, which runs inside
		// Draw/View. A click can be delivered by Update before View has
		// ever painted the current layout (e.g. right after
		// updateLayoutAndSize runs synchronously inside Update in response
		// to a session/todos event), which would leave the cached rects
		// stale or zero and silently swallow the click.
		if msg.Button == tea.MouseLeft && m.state == uiChat && m.hasSession() {
			hit := m.panelHitTest(image.Pt(msg.X, msg.Y))
			if hit.inTodosHeader {
				if cmd := m.toggleTodosExpanded(); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return cmds, true
			}
			if hit.inThreadsHeader {
				m.toggleThreadsCollapsed()
				return cmds, true
			}
			if hit.inAgentsHeader {
				m.toggleAgentsCollapsed()
				return cmds, true
			}
			if hit.threadIndex >= 0 {
				th := hit.plan.threads[hit.threadIndex]
				cmds = append(cmds, util.CmdHandler(enterThreadMsg{id: th.ID, sessionID: th.SessionID, name: th.Name}))
				return cmds, true
			}
			// A click on a delegation block drills into its child session:
			// a plain navStack push (enterChildSession), not a thread's
			// AttachThread — a delegation's session already lives in this
			// workspace.
			if hit.agentIndex >= 0 {
				if cmd := m.enterDelegation(hit.plan.agents[hit.agentIndex]); cmd != nil {
					cmds = append(cmds, cmd)
				}
				return cmds, true
			}
		}

		// Check if the click landed on an attachment's remove button.
		// The attachment chips are rendered on the first row of the
		// editor layout area, above the textarea.
		if m.activeInline == nil && msg.Button == uv.MouseLeft && len(m.editor.attachments.List()) > 0 && msg.Y == m.lay.layout.editor.Min.Y {
			relX := msg.X - m.lay.layout.editor.Min.X
			if m.editor.attachments.HandleClick(relX) {
				return cmds, true
			}
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.lay.layout.main.Min.X
			y -= m.lay.layout.main.Min.Y
			if !image.Pt(msg.X, msg.Y).In(m.lay.layout.sidebar) {
				if handled, cmd := m.chat.HandleScrollbarMouseDown(x, y); handled {
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				} else if handled, cmd := m.chat.HandleMouseDown(x, y); handled {
					m.lastClickTime = time.Now()
					if cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}
		}

	case tea.MouseMotionMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return cmds, true
		}

		// Hover feedback for the breadcrumb bar's Back button.
		m.breadcrumbHover = m.breadcrumbButtonHit(image.Pt(msg.X, msg.Y))

		if m.activeInline == nil && len(m.editor.attachments.List()) > 0 && msg.Y == m.lay.layout.editor.Min.Y {
			m.editor.attachments.SetHover(msg.X - m.lay.layout.editor.Min.X)
		} else {
			m.editor.attachments.SetHover(-1)
		}

		// Hover feedback for the session panel's todos header, thread
		// blocks, and delegation blocks, mirroring the child-session
		// panel's hover pattern above.
		if m.state == uiChat {
			hit := m.panelHitTest(image.Pt(msg.X, msg.Y))
			m.panel.todosHover = hit.inTodosHeader
			m.panel.threadsHover = hit.inThreadsHeader
			m.panel.hoveredThread = hit.threadIndex
			m.panel.agentsHover = hit.inAgentsHeader
			m.panel.hoveredAgent = hit.agentIndex
		}

		// Track hover position for inline editors.
		if m.activeInline != nil {
			if m.hoverX != msg.X || m.hoverY != msg.Y {
				m.hoverX = msg.X
				m.hoverY = msg.Y
				if clickable, ok := m.activeInline.(dialog.MouseClickableEditor); ok {
					clickable.SetHover(msg.X, msg.Y)
				}
			}
		}

		switch m.state {
		case uiChat:
			// Skip chat edge-scrolling when an inline editor is
			// active to prevent accidental scrolling while hovering
			// over question forms or other inline components.
			if m.activeInline != nil && m.focus == uiFocusEditor {
				break
			}

			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.lay.layout.main.Min.X
			y -= m.lay.layout.main.Min.Y

			// An active scrollbar drag takes over the whole gesture: it
			// tracks the cursor directly and must not also trigger the
			// text-selection edge-scroll below (the two would fight over
			// the offset).
			if handled, cmd := m.chat.HandleScrollbarMouseDrag(x, y); handled {
				if cmd != nil {
					cmds = append(cmds, cmd)
				}
				break
			}

			// Edge auto-scroll belongs to a selection drag, and works in
			// the chat's own coordinates: y, not the absolute msg.Y the
			// comparison below used to make against a chat-relative
			// height. Ungated, every pointer movement over the lower part
			// of the window scrolled the chat and snapped it to the
			// selection; measured absolutely, the bottom trigger fired
			// while the pointer was still well inside the chat.
			if !m.chat.Dragging() {
				m.chat.HandleMouseDrag(x, y)
				m.chat.HandleMouseHover(x, y)
				m.chat.ScrollbarHoverAt(x, y)
				break
			}

			if y <= 0 {
				if cmd := m.chat.ScrollByAndAnimate(-1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectPrev()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			} else if y >= m.chat.Height()-1 {
				if cmd := m.chat.ScrollByAndAnimate(1); cmd != nil {
					cmds = append(cmds, cmd)
				}
				if !m.chat.SelectedItemInView() {
					m.chat.SelectNext()
					if cmd := m.chat.ScrollToSelectedAndAnimate(); cmd != nil {
						cmds = append(cmds, cmd)
					}
				}
			}

			m.chat.HandleMouseDrag(x, y)
			m.chat.HandleMouseHover(x, y)
			m.chat.ScrollbarHoverAt(x, y)
		}

	case tea.MouseReleaseMsg:
		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return cmds, true
		}

		switch m.state {
		case uiChat:
			x, y := msg.X, msg.Y
			// Adjust for chat area position
			x -= m.lay.layout.main.Min.X
			y -= m.lay.layout.main.Min.Y
			if m.chat.HandleScrollbarMouseUp() {
				// Scrollbar drag ended; nothing else to do.
			} else if m.chat.HandleMouseUp(x, y) && m.chat.HasHighlight() {
				// Snapshot lastClickTime up front: the tea.Tick closure
				// runs on the cmd goroutine and must not read m off the
				// Update loop.
				clickTime := m.lastClickTime
				cmds = append(cmds, tea.Tick(doubleClickThreshold, func(t time.Time) tea.Msg {
					if time.Since(clickTime) >= doubleClickThreshold {
						return copyChatHighlightMsg{}
					}
					return nil
				}))
			}
		}
	case common.CoalescedWheelMsg:
		// Route wheel events to active inline editor only when the
		// mouse is over the editor area, so scrolling over the chat
		// still scrolls the chat.
		if m.activeInline != nil && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(m.lay.layout.editor) {
			if we, ok := m.activeInline.(common.WheelScrollable); ok {
				we.HandleWheel(msg.DeltaX, msg.DeltaY)
				return cmds, true
			}
		}

		// Pass mouse events to dialogs first if any are open.
		if m.dialog.HasDialogs() {
			m.dialog.Update(msg)
			return cmds, true
		}

		// Otherwise handle mouse wheel for chat. Use the coalesced delta
		// directly as the line count. Terminals like Ghostty send DeltaY=3
		// per physical wheel tick (matching their native scrollback), while
		// others send DeltaY=1.
		switch m.state {
		case uiChat:
			// When the mouse is hovering the sidebar, route wheel events to
			// sidebar scrolling. Focus never enters the sidebar (see
			// uiFocusState), so this is purely a hover check.
			if m.sidebar.scrollable && image.Pt(msg.Mouse.X, msg.Mouse.Y).In(m.lay.layout.sidebar) {
				lines := int(msg.DeltaY)
				if lines != 0 {
					seq := m.sidebar.scrollByWheel(lines)
					cmds = append(cmds, sidebarScrollbarHideCmd(m, seq))
				}
				break
			}
			// When the mouse is hovering the session panel's scrollable
			// todos rows, the wheel scrolls that section's own offset
			// instead of the chat — recomputed fresh from
			// sessionPanelRowLayout (not the m.panel.todosListRect cache),
			// same rationale as the click hit-test above: a wheel event
			// can arrive before drawSessionPanel has painted the current
			// layout.
			if m.hasSession() {
				hit := m.panelHitTest(image.Pt(msg.Mouse.X, msg.Mouse.Y))
				if hit.plan.todosScrollable && hit.inTodosList {
					lines := int(msg.DeltaY)
					if lines != 0 {
						m.panel.todosScrollOffset = clampPanelTodosScrollOffset(m.panel.todosScrollOffset+lines, hit.plan)
					}
					break
				}
			}
			if msg.DeltaX != 0 {
				m.chat.ScrollSelectedShellHorizontal(int(msg.DeltaX))
			}
			lines := int(msg.DeltaY)
			if lines == 0 {
				break
			}
			if cmd := m.chat.ScrollByAndAnimate(lines); cmd != nil {
				cmds = append(cmds, cmd)
			}
			// The wheel moves the viewport; the selection follows it into
			// the nearest message still on screen. It must not be the other
			// way round: scrolling back to the selection (what the arrow
			// keys do, where one press is one line) undoes the tick that
			// just happened, because a wheel tick is several lines and a
			// tall message holds the selection across many of them — the
			// view snapped straight back to where it started, so the wheel
			// could not scroll at all.
			if !m.chat.SelectedItemInView() {
				switch {
				case lines < 0:
					m.chat.SelectLastInView()
				case m.chat.AtBottom():
					m.chat.SelectLast()
				default:
					m.chat.SelectFirstInView()
				}
			}
		}
	}
	return cmds, false
}
