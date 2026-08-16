package model

import (
	"image"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/ultraviolet/layout"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/workspace"
)

// selectedModel returns the currently selected model as memoized by the
// off-thread busy/agent probe (see workspace_cache.go), or nil when the
// agent isn't ready. It must never probe the workspace: it is called on
// every frame, and workspace calls are treated as IO that must not run
// synchronously in Update or View (see workspace_cache.go).
func (m *UI) selectedModel() *workspace.AgentModel {
	if m.wsCache.agentReady {
		model := m.wsCache.agentModel
		return &model
	}
	return nil
}

// landingView renders the landing page view showing the current working
// directory, model information, and LSP/MCP status in a two-column layout.
func (m *UI) landingView() string {
	t := m.com.Styles
	width := m.lay.layout.main.Dx()
	cwd := common.PrettyPath(t, m.com.Workspace.WorkingDir(), width)

	parts := []string{
		cwd,
	}

	parts = append(parts, "", m.modelInfo(width))
	infoSection := lipgloss.JoinVertical(lipgloss.Left, parts...)

	var remainingHeightArea image.Rectangle
	layout.Vertical(
		layout.Len(lipgloss.Height(infoSection)+1),
		layout.Fill(1),
	).Split(m.lay.layout.main).Assign(new(image.Rectangle), &remainingHeightArea)

	mcpLspSectionWidth := min(30, (width-2)/3)

	lspSection := m.lspInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)
	mcpSection := m.mcpInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)
	skillsSection := m.skillsInfo(mcpLspSectionWidth, max(1, remainingHeightArea.Dy()), false)

	content := lipgloss.JoinHorizontal(lipgloss.Left, lspSection, " ", mcpSection, " ", skillsSection)

	return lipgloss.NewStyle().
		Width(width).
		Height(m.lay.layout.main.Dy() - 1).
		PaddingTop(1).
		Render(
			lipgloss.JoinVertical(lipgloss.Left, infoSection, "", content),
		)
}
