package model

import (
	"fmt"
	"image"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/session"
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

	// sig is the last set of inputs `content` was rendered from. See
	// sidebarSig and computeSidebarSig: updateSidebarScrollState skips the
	// render (and the BackgroundJobCounts mutex acquisition it implies)
	// whenever a freshly computed signature still equals this one.
	sig    sidebarSig
	sigSet bool
}

// sidebarSig is everything updateSidebarScrollState's rendered content
// depends on, captured cheaply (no rendering, no lipgloss) so it can be
// recomputed and compared every frame without paying for the thing it's
// trying to avoid. Two frames with an equal sig are guaranteed to render
// identical content, so the cached content/totalLines/etc. can be reused
// as-is.
//
// Every field here is either a pointer/struct that's always replaced
// wholesale when it changes (sess, so identity comparison is exact) or a
// version counter bumped at the handful of call sites that replace a map
// or slice this depends on (lsp states, mcp states, skill states, session
// files — see lspState.version, integrationsState.mcpVersion/
// skillsVersion, sessionState.filesVersion). Anything else read here is a
// cheap scalar (ints, strings, bools) copied directly, since comparing
// those is no more expensive than reading them.
type sidebarSig struct {
	area image.Rectangle // m.lay.layout.sidebar; covers width/height changes

	sess *session.Session // identity only; see sessionState doc comment
	cwd  string

	modelNil        bool
	modelProviderID string
	providerName    string
	modelName       string
	canReason       bool
	reasoningLevels int
	think           bool
	reasoningEffort string

	planKnown              bool
	plan                   string
	primaryUsedPercent     int
	primaryWindowMinutes   int
	primaryResetsAt        int64
	secondaryUsedPercent   int
	secondaryWindowMinutes int
	secondaryResetsAt      int64

	jobCounts shell.BackgroundJobCounts

	filesVersion  int
	lspVersion    int
	mcpVersion    int
	skillsVersion int
	labelsVersion int

	// theme is the palette the cached content was rendered in. Every
	// other field here is model state; this one is not, and it is the
	// input a signature over model state alone misses: setTheme swaps
	// *com.Styles in place, so the pointer is unchanged and so is
	// everything else keyed above — the sidebar went on serving content
	// painted in the previous palette until something unrelated
	// invalidated it.
	theme string
}

// computeSidebarSig reads the current values of everything
// updateSidebarScrollState's render depends on. It must stay cheap: it runs
// every frame regardless of whether the cache hits.
// sidebarScrollbarHideMsg is sent to hide the sidebar scrollbar after
// timeout. It lived in chat.go next to the chat's own version of the same
// timer, which is the only thing chat.go knew about *UI — an accident of
// placement rather than a dependency.
type sidebarScrollbarHideMsg struct {
	owner *UI // see scrollbarHideMsg.owner
	seq   int
}

// sidebarScrollbarHideCmd returns a command that sends a
// sidebarScrollbarHideMsg after the timeout.
func sidebarScrollbarHideCmd(owner *UI, seq int) tea.Cmd {
	return tea.Tick(scrollbarHideDuration, func(_ time.Time) tea.Msg {
		return sidebarScrollbarHideMsg{owner: owner, seq: seq}
	})
}

func (m *UI) computeSidebarSig() sidebarSig {
	sig := sidebarSig{
		area:      m.lay.layout.sidebar,
		sess:      m.sess.current,
		theme:     m.ops.themeLive,
		cwd:       m.com.Workspace.WorkingDir(),
		jobCounts: m.com.Workspace.BackgroundJobCounts(),

		filesVersion:  m.sess.filesVersion,
		lspVersion:    m.lsp.version,
		mcpVersion:    m.mcpVersion,
		skillsVersion: m.skillsVersion,
		labelsVersion: m.labelsVersion,
	}

	model := m.viewedModel()
	if model == nil {
		sig.modelNil = true
		return sig
	}

	sig.modelProviderID = model.ModelCfg.Provider
	sig.modelName = model.CatalogCfg.Name
	sig.canReason = model.CatalogCfg.CanReason
	sig.reasoningLevels = len(model.CatalogCfg.ReasoningLevels)
	sig.think = model.ModelCfg.Think
	sig.reasoningEffort = model.ModelCfg.ReasoningEffort
	if providerConfig, ok := m.com.Config().Providers.Get(model.ModelCfg.Provider); ok {
		sig.providerName = providerConfig.Name
	}

	if usage, ok := m.com.Workspace.CurrentPlanUsage(model.ModelCfg.Provider); ok {
		sig.planKnown = true
		sig.plan = usage.Plan
		sig.primaryUsedPercent = usage.Primary.UsedPercent
		sig.primaryWindowMinutes = usage.Primary.WindowMinutes
		sig.primaryResetsAt = usage.Primary.ResetsAt.UnixNano()
		sig.secondaryUsedPercent = usage.Secondary.UsedPercent
		sig.secondaryWindowMinutes = usage.Secondary.WindowMinutes
		sig.secondaryResetsAt = usage.Secondary.ResetsAt.UnixNano()
	}

	return sig
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
	return common.ModelInfo(m.com.Styles, modelName, providerName, reasoningInfo, m.planInfo(m.com, model), modelContext, width)
}

// planInfo describes the subscription the current model runs on and how
// much of its allowance is gone — the two things a flat-rate provider hides
// that a per-token cost line would otherwise show.
//
// Empty for everything but Codex, and empty there too until the account has
// made a request: the figures are quoted on responses, so there is nothing
// to report before the first one.
func (a *accountLabelsState) planInfo(com *common.Common, model *mcp.AgentModel) string {
	if model == nil {
		return ""
	}
	usage, ok := com.Workspace.CurrentPlanUsage(model.ModelCfg.Provider)
	if !ok {
		return ""
	}
	return common.FormatPlanUsage(usage.Plan, planWindows(usage), a.accountLabelFor(model.ModelCfg.Provider))
}

// planWindows converts the account's rate-limit windows into the shape the
// renderer takes, dropping any the plan does not have.
func planWindows(usage accounts.Usage) []common.PlanWindow {
	var windows []common.PlanWindow
	for _, w := range []accounts.UsageWindow{usage.Primary, usage.Secondary} {
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

// updateSidebarScrollState computes scroll state (scrollability, max
// offset, clamp) before drawing, re-rendering the sidebar content only when
// something it depends on actually changed. This keeps all state mutation
// in the update path rather than in the draw function.
//
// The sidebar render loops over LSP/MCP/skill state and calls
// BackgroundJobCounts (a mutex acquisition in the shell package), and this
// runs every frame — a redraw does too, TUI or not. computeSidebarSig
// captures those inputs cheaply; a frame whose signature matches the one
// `content` was last built from reuses it instead of re-rendering.
func (m *UI) updateSidebarScrollState() {
	if m.sess.current == nil || m.lay.isCompact {
		return
	}

	sig := m.computeSidebarSig()
	if m.sidebar.sigSet && sig == m.sidebar.sig {
		// Nothing that feeds the render changed; only the scroll clamp
		// below can still matter (e.g. a wheel scroll moved the offset),
		// and that reads cached content/contentHeight/totalLines, none of
		// which need recomputing.
		if m.sidebar.offset > m.sidebar.maxOffset {
			m.sidebar.offset = m.sidebar.maxOffset
		}
		return
	}
	m.sidebar.sig = sig
	m.sidebar.sigSet = true

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
	lspSection := m.lsp.lspInfo(m.com, contentWidth, len(m.lsp.states), true)
	mcpSection := m.mcpInfo(m.com, contentWidth, mcpCount(m.com.Config().MCP.Sorted(), m.mcpStates), true)
	skillsSection := m.skillsInfo(m.com, contentWidth, len(m.skillStatusItems(m.com)), true)
	filesSection := m.sess.filesInfo(m.com, m.com.Workspace.WorkingDir(), contentWidth, fileChangeCount(m.sess.files), true)

	// Build the scrollable content.
	content := lipgloss.JoinVertical(
		lipgloss.Left,
		title,
		"",
		cwd,
		"",
		m.modelInfo(contentWidth),
		"",
		backgroundJobsInfo(t, sig.jobCounts, contentWidth),
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

// hasFileChanges reports whether a session file counts as "changed" for the
// sidebar count and the "Modified Files" list: it has a non-zero diff, or
// it's uncommitted (a new/staged file can be uncommitted with no diff yet
// computed). Shared so the two surfaces never disagree on which files
// count.
func hasFileChanges(f SessionFile) bool {
	return f.Uncommitted || f.Additions != 0 || f.Deletions != 0
}

// fileChangeCount returns the number of session files that count as changed
// per hasFileChanges.
func fileChangeCount(files []SessionFile) int {
	count := 0
	for _, f := range files {
		if hasFileChanges(f) {
			count++
		}
	}
	return count
}

// truncatedMoreCount returns how many items are hidden when a list of
// `total` items is truncated to maxItems entries by showing maxItems-1 real
// items followed by one "…and N more" summary line — so N must cover
// everything past those maxItems-1, not past maxItems. Shared by the
// lsp/mcp/skills sidebar sections so their summary lines agree.
func truncatedMoreCount(total, maxItems int) int {
	return total - (maxItems - 1)
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
