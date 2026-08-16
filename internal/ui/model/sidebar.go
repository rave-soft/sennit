package model

import (
	"fmt"
	"image"
	"strings"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/logo"
	"github.com/rave-soft/sennit/internal/ui/styles"
	mcp "github.com/rave-soft/sennit/internal/workspace"
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

// hideScrollbar hides the sidebar scrollbar.
func (s *sidebarState) hideScrollbar() {
	s.scrollbarVisible = false
}

// modelInfo renders the current model information including reasoning
// settings and context usage/cost for the sidebar.
func (m *UI) modelInfo(width int) string {
	model := m.viewedModel()
	reasoningInfo := ""
	providerName := ""

	if model != nil {
		// Get provider name first
		providerConfig, ok := m.com.Config().Providers.Get(model.ModelCfg.Provider)
		if ok {
			providerName = providerConfig.Name

			// Only check reasoning if model can reason. The effort line
			// shows only an explicitly configured effort — when unset, the
			// model just runs on its default and the line is omitted
			// entirely rather than echoing that default back.
			if model.CatalogCfg.CanReason {
				if len(model.CatalogCfg.ReasoningLevels) == 0 {
					if model.ModelCfg.Think {
						reasoningInfo = "Thinking On"
					} else {
						reasoningInfo = "Thinking Off"
					}
				} else if model.ModelCfg.ReasoningEffort != "" {
					reasoningInfo = fmt.Sprintf("Reasoning %s", common.FormatReasoningEffort(model.ModelCfg.ReasoningEffort))
				}
			}
		}
	}

	var modelContext *common.ModelContextInfo
	if model != nil && m.sess.current != nil {
		modelContext = &common.ModelContextInfo{
			ContextUsed:    m.sess.current.CompletionTokens + m.sess.current.PromptTokens,
			Cost:           m.sess.current.Cost,
			ModelContext:   model.CatalogCfg.ContextWindow,
			EstimatedUsage: m.sess.current.EstimatedUsage,
		}
	}
	var modelName string
	if model != nil {
		modelName = model.CatalogCfg.Name
	}
	return common.ModelInfo(m.com.Styles, modelName, providerName, reasoningInfo, m.planInfo(model), modelContext, width)
}

// planInfo describes the subscription the current model runs on and how
// much of its allowance is gone — the two things a flat-rate provider hides
// that a per-token cost line would otherwise show.
//
// Empty for everything but Codex, and empty there too until the account has
// made a request: the figures are quoted on responses, so there is nothing
// to report before the first one.
func (m *UI) planInfo(model *mcp.AgentModel) string {
	if model == nil || model.ModelCfg.Provider != codex.ProviderID {
		return ""
	}
	usage, ok := codex.LatestUsage()
	if !ok {
		return ""
	}
	return common.FormatPlanUsage(usage.Plan, planWindows(usage))
}

// planWindows converts the account's rate-limit windows into the shape the
// renderer takes, dropping any the plan does not have.
func planWindows(usage codex.Usage) []common.PlanWindow {
	var windows []common.PlanWindow
	for _, w := range []codex.UsageWindow{usage.Primary, usage.Secondary} {
		if !w.Known() {
			continue
		}
		windows = append(windows, common.PlanWindow{
			UsedPercent:   w.UsedPercent,
			WindowMinutes: w.WindowMinutes,
			ResetsAt:      w.ResetsAt,
		})
	}
	return windows
}

func backgroundJobsInfo(t *styles.Styles, counts shell.BackgroundJobCounts, width int) string {
	header := common.Section(t, "Background Jobs", width)
	active := t.Resource.Name.Render("Active") + " " + t.Resource.CapabilityCount.Render(fmt.Sprintf("%d/%d", counts.Active, shell.MaxBackgroundJobs))
	return lipgloss.JoinVertical(lipgloss.Left, header, active)
}

// updateSidebarScrollState renders the sidebar content and computes scroll
// state (scrollability, max offset, clamp) before drawing. This keeps all
// state mutation in the update path rather than in the draw function.
func (m *UI) updateSidebarScrollState() {
	if m.sess.current == nil || m.lay.isCompact {
		return
	}

	const logoHeightBreakpoint = 30

	t := m.com.Styles
	width := m.lay.layout.sidebar.Dx()
	height := m.lay.layout.sidebar.Dy()

	contentWidth := max(width-2, 1)

	title := t.Sidebar.SessionTitle.Width(contentWidth).MaxHeight(2).Render(m.sess.current.Title)
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), contentWidth)
	sidebarLogo := m.sidebar.logo
	if height < logoHeightBreakpoint {
		sidebarLogo = lipgloss.JoinVertical(lipgloss.Left, logo.SmallRender(m.com.Styles, contentWidth, logo.Opts{}), "")
	}

	var logoRect, contentRect image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(sidebarLogo)),
		layout.Fill(1),
	).Split(m.lay.layout.sidebar).Assign(&logoRect, &contentRect)

	contentHeight := contentRect.Dy()

	// Render all items without truncation; virtual scrolling handles overflow.
	lspSection := m.lspInfo(contentWidth, len(m.lsp.states), true)
	mcpSection := m.mcpInfo(contentWidth, mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates), true)
	skillsSection := m.skillsInfo(contentWidth, len(m.skillStatusItems()), true)
	filesSection := m.filesInfo(m.com.Workspace.WorkingDir(), contentWidth, fileChangeCount(m.sess.files), true)

	// Build the scrollable content.
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		cwd,
		"",
		m.modelInfo(contentWidth),
		"",
		backgroundJobsInfo(t, m.com.Workspace.BackgroundJobCounts(), contentWidth),
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

	// Clamp sidebarOffset.
	if m.sidebar.offset > m.sidebar.maxOffset {
		m.sidebar.offset = m.sidebar.maxOffset
	}
}

// drawSidebar renders the chat sidebar with a fixed logo and a
// virtual-scrolling content area with an auto-hiding scrollbar. While the
// sidebar is focused, the scrollbar stays visible.
func (m *UI) drawSidebar(scr uv.Screen, area uv.Rectangle) {
	if m.sess.current == nil {
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

	// Determine scrollbar visibility: shown briefly after a wheel scroll,
	// then auto-hidden (see sidebarScrollbarHideMsg). The sidebar can no
	// longer hold keyboard focus, so hover/wheel is the only trigger.
	scrollbarVisible := totalLines > contentHeight && m.sidebar.scrollbarVisible

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
func mcpCount(mcpCfgs []config.MCP, states map[string]mcp.MCPClientInfo) int {
	count := 0
	for _, cfg := range mcpCfgs {
		if _, ok := states[cfg.Name]; ok {
			count++
		}
	}
	return count
}
