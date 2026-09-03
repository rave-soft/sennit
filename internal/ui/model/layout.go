package model

import (
	"image"
	"math/rand"
	"os"
	"slices"
	"strings"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/charmbracelet/ultraviolet/screen"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/home"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/dialog"
	"github.com/rave-soft/sennit/internal/ui/logo"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/version"
)

// layoutState holds the terminal dimensions and the derived compact/details
// layout mode.
type layoutState struct {
	// The width and height of the terminal in cells.
	width  int
	height int
	layout uiLayout

	// forceCompactMode tracks whether compact mode is forced by user toggle
	forceCompactMode bool

	// isCompact tracks whether we're currently in compact layout mode (either
	// by user toggle or auto-switch based on window size)
	isCompact bool

	// detailsOpen tracks whether the details panel is open (in compact mode)
	detailsOpen bool

	isTransparent bool
}

// uiLayout defines the positioning of UI elements.
type uiLayout struct {
	// area is the overall available area.
	area uv.Rectangle

	// header is the header shown in special cases
	// e.x when the sidebar is collapsed
	// or when in the landing page
	// or in init/config
	header uv.Rectangle

	// main is the area for the main pane. (e.x chat, configure, landing)
	main uv.Rectangle

	// panel is the area for the merged session panel (active threads +
	// todos + queued prompts) between chat and the editor.
	panel uv.Rectangle

	// editor is the area for the editor pane.
	editor uv.Rectangle

	// sidebar is the area for the sidebar.
	sidebar uv.Rectangle

	// status is the area for the status view.
	status uv.Rectangle

	// session details is the area for the session details overlay in compact mode.
	sessionDetails uv.Rectangle
}

// Compact mode breakpoints.
const (
	compactModeWidthBreakpoint  = 120
	compactModeHeightBreakpoint = 30
)

// Session details panel max height.
const sessionDetailsMaxHeight = 20

// sidebarColumnWidth is the sidebar's column width in the non-compact chat
// layout, including its one-column left padding. generateLayout, drawView,
// and editorContentWidth must all agree on this figure — letting them drift
// (32 / 31 / 30) clips the inline QuestionForm by a column or two.
const sidebarColumnWidth = 32

// editorAttachmentsRowHeight is the extra row the editor reserves above the
// textarea for the attachments strip. It only counts when there's something
// to show there — see (*UI).editorAttachmentsRowOffset.
const editorAttachmentsRowHeight = 1

// drawHeader draws the header section of the UI.
func (m *UI) drawHeader(scr uv.Screen, area uv.Rectangle) {
	m.header.drawHeader(
		scr,
		area,
		m.sess.current,
		m.lay.isCompact,
		m.lay.detailsOpen,
		area.Dx(),
		m.lspErrorCount(),
		m.activeThreadBadgeCount(),
		bindingKey(m.keyMap.Chat.Details),
	)
}

// drawEditorArea draws the message editor area: an active inline dialog (a
// paste preview, a question form, ...) when one is set, or the plain
// textarea view otherwise. Shared by the uiLanding and uiChat draw paths in
// Draw, which used to duplicate this exact branch and differ only in the
// width passed to renderEditorView (uiChat reserves room for the sidebar;
// see editorContentWidth).
func (m *UI) drawEditorArea(scr uv.Screen, area uv.Rectangle, editorWidth int) {
	if m.activeInline != nil {
		m.activeInline.SetFocused(m.focus == uiFocusEditor)
		if m.focus == uiFocusEditor {
			m.inlineCursor = m.activeInline.Draw(scr, area)
		} else if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && m.shouldCollapseQuestion(qf) {
			qf.DrawCollapsed(scr, area)
			m.inlineCursor = nil
		} else {
			m.inlineCursor = m.activeInline.Draw(scr, area)
		}
		return
	}
	editor := uv.NewStyledString(m.editor.renderEditorView(editorWidth))
	editor.Draw(scr, area)
	m.drawGhostText(scr)
	m.inlineCursor = nil
}

// Draw implements [uv.Drawable] and draws the UI model.
func (m *UI) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	layout := m.generateLayout(area.Dx(), area.Dy())

	if m.lay.layout != layout {
		m.lay.layout = layout
		m.updateSize()
	}

	if m.state == uiChat && m.sess.hasSession() && !m.lay.isCompact {
		m.updateSidebarScrollState()
	}

	// Clear the screen first
	screen.Clear(scr)

	switch m.state {
	case uiOnboarding:
		m.drawHeader(scr, layout.header)

		// NOTE: Onboarding flow will be rendered as dialogs below, but
		// positioned at the bottom left of the screen.

	case uiInitialize:
		m.drawHeader(scr, layout.header)

		main := uv.NewStyledString(m.initializeView())
		main.Draw(scr, layout.main)

	case uiLanding:
		m.drawHeader(scr, layout.header)
		main := uv.NewStyledString(m.landingView())
		main.Draw(scr, layout.main)

		m.drawEditorArea(scr, layout.editor, scr.Bounds().Dx())

	case uiChat:
		if m.lay.isCompact {
			m.drawHeader(scr, layout.header)
		} else {
			m.drawSidebar(scr, layout.sidebar)
		}

		m.chat.Draw(scr, layout.main)
		if layout.panel.Dy() > 0 {
			m.drawSessionPanel(scr, layout.panel)
		}

		if m.sess.viewingChildSession() {
			// The child-session info panel replaces the editor entirely
			// while a sub-agent session is being viewed — no textarea, no
			// ghost text, no cursor (see uiFocusState: focus is forced to
			// uiFocusMain for the duration).
			m.drawChildSessionPanel(scr, layout.editor)
			m.inlineCursor = nil
		} else {
			m.drawEditorArea(scr, layout.editor, m.editorContentWidth())
		}
		// Draw the input separators after the editor so its content cannot
		// cover the reserved boundary rows.
		m.drawChatSeparators(scr, layout.editor)

		// Draw details overlay in compact mode when open
		if m.lay.isCompact && m.lay.detailsOpen {
			m.drawSessionDetails(scr, layout.sessionDetails)
		}
	}

	isOnboarding := m.state == uiOnboarding

	// Add status and help layer
	m.status.SetHideHelp(isOnboarding)
	m.status.Draw(scr, layout.status)

	// Draw completions popup if open
	if !isOnboarding && m.editor.completions.open && m.editor.completions.popup.HasItems() {
		w, h := m.editor.completions.popup.Size()
		x := m.editor.completions.anchor.X
		y := m.editor.completions.anchor.Y - h

		screenW := area.Dx()
		if x+w > screenW {
			x = screenW - w
		}
		x = max(0, x)
		y = max(0, y+m.editorAttachmentsRowOffset())

		completionsView := uv.NewStyledString(m.editor.completions.popup.Render())
		completionsView.Draw(scr, image.Rectangle{
			Min: image.Pt(x, y),
			Max: image.Pt(x+w, y+h),
		})
	}

	// Debugging rendering (visually see when the tui rerenders)
	if os.Getenv(brand.EnvPrefix+"UI_DEBUG") == "true" {
		debugView := lipgloss.NewStyle().Background(lipgloss.ANSIColor(rand.Intn(256))).Width(4).Height(2)
		debug := uv.NewStyledString(debugView.String())
		debug.Draw(scr, image.Rectangle{
			Min: image.Pt(4, 1),
			Max: image.Pt(8, 3),
		})
	}

	// This needs to come last to overlay on top of everything. We always pass
	// the full screen bounds because the dialogs will position themselves
	// accordingly.
	if m.dialog.HasDialogs() {
		return m.dialog.Draw(scr, scr.Bounds())
	}

	switch m.focus {
	case uiFocusEditor:
		if m.lay.layout.editor.Dy() <= 0 {
			// Don't show cursor if editor is not visible
			return nil
		}
		if m.lay.detailsOpen && m.lay.isCompact {
			// Don't show cursor if details overlay is open
			return nil
		}

		if m.activeInline != nil {
			if cur := m.inlineCursor; cur != nil {
				cur.X++                            // Adjust for app margins
				cur.Y += m.lay.layout.editor.Min.Y // Inline editor draws from area top
				return cur
			}
			return nil
		}

		if m.editor.textarea.Focused() {
			cur := m.editor.textarea.Cursor()
			cur.X++ // Adjust for app margins
			cur.Y += m.lay.layout.editor.Min.Y + m.editorAttachmentsRowOffset()
			return cur
		}
	}
	return nil
}

func (m *UI) drawChatSeparators(scr uv.Screen, editorArea uv.Rectangle) {
	if editorArea.Dx() <= 0 {
		return
	}
	separator := m.com.Styles.Messages.ChatSeparator.Render(
		strings.Repeat(styles.SectionSeparator, editorArea.Dx()),
	)
	for _, y := range []int{editorArea.Min.Y - 1, editorArea.Max.Y} {
		if y < scr.Bounds().Min.Y || y >= scr.Bounds().Max.Y {
			continue
		}
		row := image.Rect(editorArea.Min.X, y, editorArea.Max.X, y+1)
		// The rule above the editor doubles as the navigation bar once
		// there is a trail to show; it falls through to the plain rule at
		// the top level and on terminals too narrow for it.
		if y == editorArea.Min.Y-1 && m.drawBreadcrumbBar(scr, row) {
			continue
		}
		uv.NewStyledString(separator).Draw(scr, row)
	}
}

// View renders the UI model's view.
func (m *UI) View() tea.View {
	var v tea.View
	v.AltScreen = true
	if !m.lay.isTransparent {
		v.BackgroundColor = m.com.Styles.Background
	}
	v.MouseMode = tea.MouseModeAllMotion
	v.ReportFocus = m.caps.ReportFocusEvents
	v.WindowTitle = brand.Slug + " " + home.Short(m.com.Workspace.WorkingDir())
	if m.sess.hasSession() && m.sess.current.Title != "" {
		v.WindowTitle += " — " + m.sess.current.Title
	}

	canvas := uv.NewScreenBuffer(m.lay.width, m.lay.height)
	v.Cursor = m.Draw(canvas, canvas.Bounds())

	content := strings.ReplaceAll(canvas.Render(), "\r\n", "\n") // normalize newlines
	contentLines := strings.Split(content, "\n")
	for i, line := range contentLines {
		// Trim trailing spaces for concise rendering
		contentLines[i] = strings.TrimRight(line, " ")
	}

	content = strings.Join(contentLines, "\n")

	v.Content = content
	if m.progressBarEnabled && m.sendProgressBar && m.isAgentBusy() {
		// HACK: use a random percentage to prevent ghostty from hiding it
		// after a timeout.
		v.ProgressBar = tea.NewProgressBar(tea.ProgressBarIndeterminate, rand.Intn(100))
	}

	return v
}

// updateLayoutAndSize updates the layout and sizes of UI components.
func (m *UI) updateLayoutAndSize() {
	// Determine if we should be in compact mode
	if m.state == uiChat {
		if m.lay.forceCompactMode {
			m.lay.isCompact = true
		} else if m.lay.width < compactModeWidthBreakpoint || m.lay.height < compactModeHeightBreakpoint {
			m.lay.isCompact = true
		} else {
			m.lay.isCompact = false
		}
	}

	// First pass sizes components from the current textarea height.
	m.lay.layout = m.generateLayout(m.lay.width, m.lay.height)
	prevHeight := m.editor.textarea.Height()
	m.updateSize()

	// SetWidth can change textarea height due to soft-wrap recalculation.
	// If that happens, run one reconciliation pass with the new height.
	if m.editor.textarea.Height() != prevHeight {
		m.lay.layout = m.generateLayout(m.lay.width, m.lay.height)
		m.updateSize()
	}
}

// updateSize updates the sizes of UI components based on the current layout.
func (m *UI) updateSize() {
	// Set status width
	m.status.SetWidth(m.lay.layout.status.Dx())

	m.chat.SetSize(m.lay.layout.main.Dx(), m.lay.layout.main.Dy())
	m.editor.textarea.MaxHeight = TextareaMaxHeight
	m.editor.textarea.SetWidth(m.lay.layout.editor.Dx())

	// Handle different app states
	switch m.state {
	case uiChat:
		if !m.lay.isCompact {
			m.sidebar.cacheSidebarLogo(m.com, m.lay.layout.sidebar.Dx())
		}
	}
}

// splitMainMinusEditorAndPanel takes mainRect — the content area left after
// the header/sidebar split — and carves out the editor strip (editorHeight
// rows, with the one-row top/bottom padding the compact and sidebar
// branches both reserve), then, from what's left, the session panel's rows
// when it wants any. Returns the (possibly panel-shrunk) chat rect, the
// editor rect, and the panel rect (zero when the panel has nothing to
// show). Shared by generateLayout's compact and sidebar uiChat branches,
// which used to duplicate this exact split.
func (m *UI) splitMainMinusEditorAndPanel(mainRect image.Rectangle, editorHeight int) (main, editor, panel image.Rectangle) {
	var editorRect image.Rectangle
	layout.Vertical(
		layout.Len(max(0, mainRect.Dy()-editorHeight-2)),
		layout.Len(1),
		layout.Len(editorHeight),
		layout.Len(1),
	).Split(mainRect).Assign(&mainRect, new(image.Rectangle), &editorRect, new(image.Rectangle))
	mainRect.Max.X -= 1 // Add padding right

	panelHeight := m.sessionPanelHeight(mainRect.Dy() + 2)
	if panelHeight <= 0 {
		return mainRect, editorRect, image.Rectangle{}
	}
	var chatRect, panelRect image.Rectangle
	layout.Vertical(
		layout.Len(mainRect.Dy()-panelHeight),
		layout.Fill(1),
	).Split(mainRect).Assign(&chatRect, &panelRect)
	return chatRect, editorRect, panelRect
}

// generateLayout calculates the layout rectangles for all UI components based
// on the current UI state and terminal dimensions.
func (m *UI) generateLayout(w, h int) uiLayout {
	// The screen area we're working with
	area := image.Rect(0, 0, w, h)

	// The help height
	helpHeight := 1
	// The editor height: textarea height, plus one row for the attachments
	// strip when there are attachments to show. No fixed bottom margin — a
	// one-line, no-attachments editor is exactly one row.
	// When an inline editor is active, use its height instead.
	editorHeight := m.editor.textarea.Height() + m.editorAttachmentsRowOffset()
	if m.activeInline != nil {
		// The editor content width depends only on terminal width
		// and layout (not on editor height), so passing the current
		// frame's width to Height() keeps layout in sync with the
		// width Draw will use, preventing flicker during fast resize.
		editorWidth := m.editorContentWidth()
		if m.focus == uiFocusEditor {
			editorHeight = m.activeInline.Height(editorWidth)
		} else if qf, ok := m.activeInline.(*dialog.QuestionForm); ok && m.shouldCollapseQuestion(qf) {
			editorHeight = qf.CollapsedHeight() + 1
		} else {
			editorHeight = m.activeInline.Height(editorWidth)
		}
	} else if m.sess.viewingChildSession() {
		// The child-session info panel (drawChildSessionPanel) replaces the
		// textarea entirely while viewing a sub-agent session — it has its
		// own fixed, compact height instead of the textarea's.
		editorHeight = childSessionPanelHeight
	}
	// The sidebar width
	sidebarWidth := sidebarColumnWidth
	// The header height
	const landingHeaderHeight = 4

	var helpKeyMap help.KeyMap = m
	if m.status != nil && m.status.ShowingAll() {
		for _, row := range helpKeyMap.FullHelp() {
			helpHeight = max(helpHeight, len(row))
		}
	}

	// Add app margins
	var appRect, helpRect image.Rectangle
	layout.Vertical(
		layout.Len(area.Dy()-helpHeight),
		layout.Fill(1),
	).Split(area).Assign(&appRect, &helpRect)
	appRect.Min.Y += 1
	appRect.Max.Y -= 1
	helpRect.Min.Y -= 1
	appRect.Min.X += 1
	appRect.Max.X -= 1

	if slices.Contains([]uiState{uiOnboarding, uiInitialize, uiLanding}, m.state) {
		// extra padding on left and right for these states
		appRect.Min.X += 1
		appRect.Max.X -= 1
	}

	uiLayout := uiLayout{
		area:   area,
		status: helpRect,
	}

	// Handle different app states
	switch m.state {
	case uiOnboarding, uiInitialize:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// help

		var headerRect, mainRect image.Rectangle
		layout.Vertical(
			layout.Len(landingHeaderHeight),
			layout.Fill(1),
		).Split(appRect).Assign(&headerRect, &mainRect)
		uiLayout.header = headerRect
		uiLayout.main = mainRect

	case uiLanding:
		// Layout
		//
		// header
		// ------
		// main
		// ------
		// editor
		// ------
		// help
		var headerRect, mainRect image.Rectangle
		layout.Vertical(
			layout.Len(landingHeaderHeight),
			layout.Fill(1),
		).Split(appRect).Assign(&headerRect, &mainRect)
		var editorRect image.Rectangle
		layout.Vertical(
			layout.Len(mainRect.Dy()-editorHeight),
			layout.Fill(1),
		).Split(mainRect).Assign(&mainRect, &editorRect)
		// Remove extra padding from editor (but keep it for header and main)
		editorRect.Min.X -= 1
		editorRect.Max.X += 1
		uiLayout.header = headerRect
		uiLayout.main = mainRect
		uiLayout.editor = editorRect

	case uiChat:
		if m.lay.isCompact {
			// Layout
			//
			// compact-header
			// ------
			// main
			// ------
			// editor
			// ------
			// help
			const compactHeaderHeight = 1
			var headerRect, mainRect image.Rectangle
			layout.Vertical(
				layout.Len(compactHeaderHeight),
				layout.Fill(1),
			).Split(appRect).Assign(&headerRect, &mainRect)
			detailsHeight := min(sessionDetailsMaxHeight, area.Dy()-1) // One row for the header
			var sessionDetailsArea image.Rectangle
			layout.Vertical(
				layout.Len(detailsHeight),
				layout.Fill(1),
			).Split(appRect).Assign(&sessionDetailsArea, new(image.Rectangle))
			uiLayout.sessionDetails = sessionDetailsArea
			uiLayout.sessionDetails.Min.Y += compactHeaderHeight // adjust for header
			// Add one line gap between header and main content
			mainRect.Min.Y += 1
			uiLayout.header = headerRect
			uiLayout.main, uiLayout.editor, uiLayout.panel = m.splitMainMinusEditorAndPanel(mainRect, editorHeight)
		} else {
			// Layout
			//
			// ------|---
			// main  |
			// ------| side
			// editor|
			// ----------
			// help

			var mainRect, sideRect image.Rectangle
			layout.Horizontal(
				layout.Len(appRect.Dx()-sidebarWidth),
				layout.Fill(1),
			).Split(appRect).Assign(&mainRect, &sideRect)
			// Add padding left
			sideRect.Min.X += 1
			uiLayout.sidebar = sideRect
			uiLayout.main, uiLayout.editor, uiLayout.panel = m.splitMainMinusEditorAndPanel(mainRect, editorHeight)
		}
	}

	return uiLayout
}

// activeGhostTail returns the acceptable/renderable suffix of the current
// history-based ghost prediction for the editor, or "" if none should be
// shown right now (wrong focus, completions open, bang mode, empty input,
// no match, or hidden by a preceding Esc).
func (m *UI) activeGhostTail() string {
	if m.focus != uiFocusEditor || m.editor.completions.open || m.editor.bang.isActive() {
		return ""
	}
	value := m.editor.textarea.Value()
	if value == "" {
		return ""
	}
	full := m.editor.ghostSuggestionFor(value)
	if full == "" || m.editor.escape.hidesGhostFor(value) {
		return ""
	}
	return full[len(value):]
}

// drawGhostText overlays the inline history prediction (if any) onto the
// editor at the cursor position, on top of the already-drawn textarea. It
// is a pure overlay: it only paints the cells for the predicted text and
// never affects layout/height.
func (m *UI) drawGhostText(scr uv.Screen) {
	tail := m.activeGhostTail()
	if tail == "" {
		return
	}
	cur := m.editor.textarea.Cursor()
	if cur == nil {
		return
	}
	// Same cursor-to-screen mapping as completionsPosition(): textarea
	// cursor coordinates offset by the editor layout rect's origin, plus
	// editorAttachmentsRowOffset() for the attachments line renderEditorView
	// conditionally prepends above the textarea (mirrors the offset applied
	// where the completions popup position is used, above).
	x := m.lay.layout.editor.Min.X + cur.X
	y := m.lay.layout.editor.Min.Y + cur.Y + m.editorAttachmentsRowOffset()
	if x >= m.lay.layout.editor.Max.X || y >= m.lay.layout.editor.Max.Y {
		return
	}

	// Only the first line of a multi-line prediction is shown, with a
	// trailing "…" marker to indicate there's more; Tab still accepts the
	// full multi-line tail regardless of what's rendered here.
	display := tail
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 {
		display = tail[:idx] + "…"
	}

	ghost := uv.NewStyledString(m.com.Styles.Editor.Textarea.Focused.Placeholder.Render(display))
	ghost.Tail = "…" // mark truncation if it overflows the editor width
	ghost.Draw(scr, image.Rectangle{
		Min: image.Pt(x, y),
		Max: image.Pt(m.lay.layout.editor.Max.X, y+1),
	})
}

// completionsPosition returns the X and Y position for the completions popup.
func (m *UI) completionsPosition() image.Point {
	cur := m.editor.textarea.Cursor()
	if cur == nil {
		return image.Point{
			X: m.lay.layout.editor.Min.X,
			Y: m.lay.layout.editor.Min.Y,
		}
	}
	return image.Point{
		X: cur.X + m.lay.layout.editor.Min.X,
		Y: m.lay.layout.editor.Min.Y + cur.Y,
	}
}

// renderEditorView renders the editor view with attachments if any. The
// attachments strip only takes a row when there's something to show — an
// empty editor with a single line of text is exactly one row tall, with no
// reserved blank lines above or below (see editorAttachmentsRowOffset and
// TextareaMinHeight).
func (e *editorState) renderEditorView(width int) string {
	if len(e.attachments.List()) == 0 {
		return e.textarea.View()
	}
	return strings.Join([]string{
		e.attachments.Render(width),
		e.textarea.View(),
	}, "\n")
}

// editorAttachmentsRowOffset returns the number of rows renderEditorView
// prepends above the textarea for the attachments strip: 1 when there are
// attachments to show, 0 otherwise. Every consumer that maps a textarea-
// relative coordinate (cursor, ghost text, completions popup) onto the
// editor's screen rect must add this, since renderEditorView's attachments
// row is conditional rather than a fixed margin.
func (m *UI) editorAttachmentsRowOffset() int {
	// generateLayout (via updateLayoutAndSize) can run before
	// editor.attachments is wired up in some construction paths (notably
	// test harnesses that only need layout, not the full editor), so this
	// stays nil-safe unlike the Draw-time attachments call sites below,
	// which only ever run once the editor is fully set up.
	if m.editor.attachments != nil && len(m.editor.attachments.List()) > 0 {
		return editorAttachmentsRowHeight
	}
	return 0
}

// cacheSidebarLogo renders and caches the sidebar logo at the specified width.
func (sb *sidebarState) cacheSidebarLogo(com *common.Common, width int) {
	sb.logo = renderLogo(com.Styles, true, width)
}

// editorContentWidth returns the content width available to the
// editor area for the current state. It depends only on terminal
// width and layout (not on editor height), so it can be computed
// before the editor's height is known. This is the single source
// of truth for the inline editor width used by both layout sizing
// and Height() queries.
func (m *UI) editorContentWidth() int {
	width := m.lay.width - 2 // appRect horizontal margins
	if m.state == uiChat && !m.lay.isCompact {
		width -= sidebarColumnWidth
	}
	return width
}

// completionsMaxWidth caps the "/"/"@" completions popup so it never
// outgrows the terminal: 60% of the terminal width, but no wider than the
// editor's own content width, since completionsPosition anchors the popup
// to the editor's cursor column.
func (m *UI) completionsMaxWidth() int {
	maxW := m.lay.width * 6 / 10
	if ew := m.editorContentWidth(); ew > 0 && ew < maxW {
		maxW = ew
	}
	return maxW
}

// drawSessionDetails draws the session details in compact mode.
func (m *UI) drawSessionDetails(scr uv.Screen, area uv.Rectangle) {
	if m.sess.current == nil {
		return
	}

	s := m.com.Styles

	width := area.Dx() - s.CompactDetails.View.GetHorizontalFrameSize()
	height := area.Dy() - s.CompactDetails.View.GetVerticalFrameSize()

	title := s.CompactDetails.Title.Width(width).MaxHeight(2).Render(m.sess.current.Title)
	blocks := []string{
		title,
		"",
		m.modelInfo(width),
		"",
	}

	detailsHeader := lipgloss.JoinVertical(
		lipgloss.Left,
		blocks...,
	)

	version := s.CompactDetails.Version.Width(width).AlignHorizontal(lipgloss.Right).Render(version.Version)

	remainingHeight := height - lipgloss.Height(detailsHeader) - lipgloss.Height(version)

	const maxSectionWidth = 50
	sectionWidth := max(1, min(maxSectionWidth, width/4-2)) // account for spacing between sections
	maxItemsPerSection := remainingHeight - 3               // Account for section title and spacing

	lspSection := m.lsp.lspInfo(m.com, sectionWidth, maxItemsPerSection, false)
	mcpSection := m.mcpInfo(m.com, sectionWidth, maxItemsPerSection, false)
	skillsSection := m.skillsInfo(m.com, sectionWidth, maxItemsPerSection, false)
	filesSection := m.sess.filesInfo(m.com, m.com.Workspace.WorkingDir(), sectionWidth, maxItemsPerSection, false)
	sections := lipgloss.JoinHorizontal(lipgloss.Top, filesSection, " ", lspSection, " ", mcpSection, " ", skillsSection)
	uv.NewStyledString(
		s.CompactDetails.View.
			Width(area.Dx()).
			Render(
				lipgloss.JoinVertical(
					lipgloss.Left,
					detailsHeader,
					sections,
					version,
				),
			),
	).Draw(scr, area)
}

// renderLogo renders the Sennit logo with the given styles and dimensions.
func renderLogo(t *styles.Styles, compact bool, width int) string {
	return logo.Render(t.Logo.GradCanvas, version.Version, compact, logo.Opts{
		FieldColor:   t.Logo.FieldColor,
		TitleColorA:  t.Logo.TitleColorA,
		TitleColorB:  t.Logo.TitleColorB,
		VendorColor:  t.Logo.VendorColor,
		VersionColor: t.Logo.VersionColor,
		Width:        width,
	})
}
