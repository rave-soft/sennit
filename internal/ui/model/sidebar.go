package model

import (
	"cmp"
	"fmt"
	"image"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	mcp "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/logo"
)

// sidebarState holds virtual-scroll state and cached rendered content for
// the chat sidebar.
type sidebarState struct {
	// logo keeps a cached version of the sidebar logo.
	logo string

	// Scroll state for virtual scrolling.
	offset           int  // current scroll offset in lines
	scrollable       bool // true when sidebar content exceeds available height
	scrollbarVisible bool
	scrollbarSeq     int    // sequence number for auto-hide timer
	maxOffset        int    // max scroll offset, computed in updateSidebarScrollState
	content          string // cached rendered sidebar content
	totalLines       int    // total lines in content
	contentHeight    int    // available height for sidebar content
	contentWidth     int    // available width for sidebar content
	drawLogo         string // logo to render (may differ from logo for short heights)
}

// scrollByWheel adjusts the scroll offset by delta lines from a mouse wheel
// event, clamps it to the valid range, and shows the scrollbar. It returns
// the new scrollbar sequence number so the caller can arm the auto-hide
// timer.
func (s *sidebarState) scrollByWheel(lines int) int {
	s.offset = max(0, min(s.offset+lines, s.maxOffset))
	s.scrollbarSeq++
	s.scrollbarVisible = true
	return s.scrollbarSeq
}

// pageUp scrolls the sidebar up by n lines (keyboard navigation).
func (s *sidebarState) pageUp(n int) {
	s.offset = max(0, s.offset-n)
	s.scrollbarSeq++
}

// pageDown scrolls the sidebar down by n lines, clamped to maxOffset
// (keyboard navigation).
func (s *sidebarState) pageDown(n int) {
	if s.offset < s.maxOffset {
		s.offset = min(s.offset+n, s.maxOffset)
		s.scrollbarSeq++
	}
}

// toHome scrolls the sidebar to the top.
func (s *sidebarState) toHome() {
	s.offset = 0
	s.scrollbarSeq++
}

// toEnd scrolls the sidebar to the bottom.
func (s *sidebarState) toEnd() {
	s.offset = s.maxOffset
	s.scrollbarSeq++
}

// hideScrollbar hides the sidebar scrollbar.
func (s *sidebarState) hideScrollbar() {
	s.scrollbarVisible = false
}

// modelInfo renders the current model information including reasoning
// settings and context usage/cost for the sidebar.
func (m *UI) modelInfo(width int) string {
	model := m.selectedLargeModel()
	reasoningInfo := ""
	providerName := ""

	if model != nil {
		// Get provider name first
		providerConfig, ok := m.com.Config().Providers.Get(model.ModelCfg.Provider)
		if ok {
			providerName = providerConfig.Name

			// Only check reasoning if model can reason
			if model.CatalogCfg.CanReason {
				if len(model.CatalogCfg.ReasoningLevels) == 0 {
					if model.ModelCfg.Think {
						reasoningInfo = "Thinking On"
					} else {
						reasoningInfo = "Thinking Off"
					}
				} else {
					reasoningEffort := cmp.Or(model.ModelCfg.ReasoningEffort, model.CatalogCfg.DefaultReasoningEffort)
					reasoningInfo = fmt.Sprintf("Reasoning %s", common.FormatReasoningEffort(reasoningEffort))
				}
			}
		}
	}

	var modelContext *common.ModelContextInfo
	if model != nil && m.session != nil {
		modelContext = &common.ModelContextInfo{
			ContextUsed:    m.session.CompletionTokens + m.session.PromptTokens,
			Cost:           m.session.Cost,
			ModelContext:   model.CatalogCfg.ContextWindow,
			EstimatedUsage: m.session.EstimatedUsage,
		}
	}
	var modelName string
	if model != nil {
		modelName = model.CatalogCfg.Name
	}
	return common.ModelInfo(m.com.Styles, modelName, providerName, reasoningInfo, modelContext, width)
}

// updateSidebarScrollState renders the sidebar content and computes scroll
// state (scrollability, max offset, clamp) before drawing. This keeps all
// state mutation in the update path rather than in the draw function.
func (m *UI) updateSidebarScrollState() {
	if m.session == nil || m.isCompact {
		return
	}

	const logoHeightBreakpoint = 30

	t := m.com.Styles
	width := m.layout.sidebar.Dx()
	height := m.layout.sidebar.Dy()

	contentWidth := max(width-2, 1)

	title := t.Sidebar.SessionTitle.Width(contentWidth).MaxHeight(2).Render(m.session.Title)
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), contentWidth)
	sidebarLogo := m.sidebar.logo
	if height < logoHeightBreakpoint {
		sidebarLogo = lipgloss.JoinVertical(lipgloss.Left, logo.SmallRender(m.com.Styles, contentWidth, logo.Opts{}), "")
	}

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarLogo)),
		layout.Fill(1),
	).Split(m.layout.sidebar).Assign(&logoRect, &contentRect)

	contentHeight := contentRect.Dy()

	// Render all items without truncation; virtual scrolling handles overflow.
	lspSection := m.lspInfo(contentWidth, len(m.lspStates), true)
	mcpSection := m.mcpInfo(contentWidth, mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates), true)
	skillsSection := m.skillsInfo(contentWidth, len(m.skillStatusItems()), true)
	filesSection := m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, fileChangeCount(m.sessionFiles), true)

	// Build the scrollable content.
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		cwd,
		"",
		m.modelInfo(contentWidth),
		"",
		filesSection,
		"",
		lspSection,
		"",
		mcpSection,
		"",
		skillsSection,
	)

	totalLines := strings.Count(content, "\n") + 1
	m.sidebar.content = content
	m.sidebar.totalLines = totalLines
	m.sidebar.contentWidth = contentWidth
	m.sidebar.contentHeight = contentHeight
	m.sidebar.drawLogo = sidebarLogo
	m.sidebar.scrollable = totalLines > contentHeight
	m.sidebar.maxOffset = max(0, totalLines-contentHeight)

	// If the sidebar is focused but no longer scrollable (e.g. after a
	// resize), return focus to the chat.
	if m.focus == uiFocusSidebar && !m.sidebar.scrollable {
		m.focus = uiFocusMain
		m.chat.Focus()
	}

	// Clamp sidebarOffset.
	if m.sidebar.offset > m.sidebar.maxOffset {
		m.sidebar.offset = m.sidebar.maxOffset
	}
}

// drawSidebar renders the chat sidebar with a fixed logo and a
// virtual-scrolling content area with an auto-hiding scrollbar. While the
// sidebar is focused, the scrollbar stays visible.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.session == nil {
		return
	}

	sidebarLogo := m.sidebar.drawLogo
	contentWidth := m.sidebar.contentWidth
	contentHeight := m.sidebar.contentHeight
	totalLines := m.sidebar.totalLines

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarLogo)),
		layout.Fill(1),
	).Split(area).Assign(&logoRect, &contentRect)

	// Slice visible lines.
	end := min(m.sidebar.offset+contentHeight, totalLines)
	lines := strings.Split(m.sidebar.content, "\n")
	visibleLines := lines[m.sidebar.offset:end]
	visibleStr := strings.Join(visibleLines, "\n")

	// Determine scrollbar visibility: always visible when focused, otherwise
	// auto-hide.
	scrollbarVisible := totalLines > contentHeight && (m.sidebar.scrollbarVisible || m.focus == uiFocusSidebar)

	// Draw the fixed logo.
	uv.NewStyledString(
		lipgloss.NewStyle().
			MaxWidth(contentWidth).
			MaxHeight(lipgloss.Height(sidebarLogo)).
			Render(sidebarLogo),
	).Draw(scr, logoRect)

	// Draw the visible content in the scrollable area.
	uv.NewStyledString(
		lipgloss.NewStyle().
			MaxWidth(contentWidth).
			MaxHeight(contentHeight).
			Render(visibleStr),
	).Draw(scr, contentRect)

	// Draw scrollbar in the reserved column.
	if scrollbarVisible {
		scrollbar := common.Scrollbar(m.com.Styles, contentHeight, totalLines, contentHeight, m.sidebar.offset)
		if scrollbar != "" {
			scrollbarArea := image.Rectangle{
				Min: image.Point{X: area.Max.X - 1, Y: contentRect.Min.Y},
				Max: image.Point{X: area.Max.X, Y: area.Max.Y},
			}
			uv.NewStyledString(scrollbar).Draw(scr, scrollbarArea)
		}
	}
}

// fileChangeCount returns the number of session files with non-zero additions
// or deletions.
func fileChangeCount(files []SessionFile) int {
	count := 0
	for _, f := range files {
		if f.Additions == 0 && f.Deletions == 0 {
			continue
		}
		count++
	}
	return count
}

// mcpCount returns the number of MCP servers that have a state entry.
func mcpCount(mcpCfgs []config.MCP, states map[string]mcp.ClientInfo) int {
	count := 0
	for _, cfg := range mcpCfgs {
		if _, ok := states[cfg.Name]; ok {
			count++
		}
	}
	return count
}
