package styles

import "charm.land/lipgloss/v2"

// quickStyleSection fills in Section.
func quickStyleSection(s *Styles, o quickStyleOpts, base, _, subtle lipgloss.Style) {
	s.Section.Title = subtle
	s.Section.Line = base.Foreground(o.separator)
}

// quickStyleThreads fills in Threads, the threads dashboard. This is an
// operations screen — a list of live work someone is about to act on — so
// unlike the chat's chrome it leans on state being readable at a glance:
// each status class gets a distinct color rather than the one muted tone
// Status.*Message collapses to, and the toolbar's buttons carry a real
// fill so they read as pressable.
func quickStyleThreads(s *Styles, o quickStyleOpts, base, muted, subtle lipgloss.Style) {
	s.Threads.Title = base.Bold(true)
	s.Threads.Subtle = muted
	s.Threads.Rule = base.Foreground(o.separator)
	s.Threads.ColumnHeader = subtle
	s.Threads.RowBase = base
	s.Threads.RowSelected = lipgloss.NewStyle().Foreground(o.fgBase).Background(o.bgHover)
	s.Threads.StatusRunning = base.Foreground(o.accent)
	s.Threads.StatusIdle = muted
	s.Threads.StatusDone = base.Foreground(o.ansiGreen)
	s.Threads.StatusWarn = base.Foreground(o.warning)
	s.Threads.StatusError = base.Foreground(o.error)
	s.Threads.TabActive = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.primary).Padding(0, 1)
	s.Threads.TabInactive = muted.Padding(0, 1)
	s.Threads.ButtonIdle = lipgloss.NewStyle().Foreground(o.fgBase).Background(o.bgLessVisible).Padding(0, 1)
	s.Threads.ButtonHover = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.accent).Padding(0, 1)
	s.Threads.ButtonDanger = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.destructive).Padding(0, 1)
	s.Threads.ButtonDisabled = lipgloss.NewStyle().Foreground(o.fgMostSubtle).Background(o.bgLeastVisible).Padding(0, 1)
	s.Threads.DetailLabel = subtle
	s.Threads.DetailValue = base
}

// quickStyleChildBanner fills in ChildBanner, the child-session panel (see
// UI.drawChildSessionPanel): a muted breadcrumb trail leading up to a
// bold, accent-colored subagent name — the "where am I" cue — plus a
// solid-fill button styled like a real dialog button (see s.Button.
// Focused/Hovered) rather than an underlined text link, so "back" reads
// as clickable at a glance.
func quickStyleChildBanner(s *Styles, o quickStyleOpts, base, muted, _ lipgloss.Style) {
	s.ChildBanner.Base = muted
	s.ChildBanner.Path = muted
	s.ChildBanner.Sep = muted.Faint(true)
	s.ChildBanner.Current = base.Foreground(o.primary).Bold(true)
	s.ChildBanner.Button = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.secondary).Bold(true).Padding(0, 1)
	s.ChildBanner.ButtonHover = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.primary).Bold(true).Padding(0, 1)
}

// quickStyleInitialize fills in Initialize.
func quickStyleInitialize(s *Styles, o quickStyleOpts, base, muted, _ lipgloss.Style) {
	s.Initialize.Header = base
	s.Initialize.Content = muted
	s.Initialize.Accent = base.Foreground(o.successMostSubtle)
}

// quickStyleResource fills in Resource, the LSP/MCP/skills sidebar lists.
func quickStyleResource(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Resource.Heading = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Resource.Name = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Resource.StatusText = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Resource.OfflineIcon = lipgloss.NewStyle().Foreground(o.bgMostVisible).SetString("●")
	s.Resource.BusyIcon = s.Resource.OfflineIcon.Foreground(o.busy)
	s.Resource.ErrorIcon = s.Resource.OfflineIcon.Foreground(o.destructive)
	s.Resource.OnlineIcon = s.Resource.OfflineIcon.Foreground(o.successMostSubtle)
	s.Resource.EnabledIcon = s.Resource.OfflineIcon.Foreground(o.ansiGreen)
	s.Resource.NeedsAuthIcon = s.Resource.OfflineIcon.Foreground(o.attention)
	s.Resource.DisabledIcon = lipgloss.NewStyle().Foreground(o.fgMoreSubtle).SetString("●")
	s.Resource.AdditionalText = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Resource.CapabilityCount = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Resource.RowTitleBase = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Resource.RowDescBase = lipgloss.NewStyle().Foreground(o.fgBase)
	// The pre-split quickStyle assigned these two fields a second time,
	// lower down under a second "// ResourceGroup" comment, with the same
	// values both times. Removed here: the duplicate was a harmless no-op
	// in the original function, but a genuine second assignment site would
	// defeat the assigned-exactly-once check the split relies on.
	s.Resource.DefaultTitleFg = o.fgMoreSubtle
	s.Resource.DefaultDescFg = o.fgMostSubtle
}

// quickStyleLSP fills in LSP.
func quickStyleLSP(s *Styles, o quickStyleOpts, base, _, _ lipgloss.Style) {
	s.LSP.ErrorDiagnostic = base.Foreground(o.error)
	s.LSP.WarningDiagnostic = base.Foreground(o.warningSubtle)
	s.LSP.HintDiagnostic = base.Foreground(o.fgSubtle)
	s.LSP.InfoDiagnostic = base.Foreground(o.info)
	s.LSP.CleanDiagnostic = base.Foreground(o.successMostSubtle)
}

// quickStyleFiles fills in Files.
func quickStyleFiles(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Files.Path = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Files.Additions = lipgloss.NewStyle().Foreground(o.successMostSubtle)
	s.Files.Deletions = lipgloss.NewStyle().Foreground(o.error)
	s.Files.SectionTitle = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Files.EmptyMessage = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Files.TruncationHint = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
}

// quickStyleSidebar fills in Sidebar.
func quickStyleSidebar(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.Sidebar.SessionTitle = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Sidebar.WorkingDir = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
}

// quickStyleModelInfo fills in ModelInfo.
func quickStyleModelInfo(s *Styles, o quickStyleOpts, _, _, _ lipgloss.Style) {
	s.ModelInfo.Icon = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.ModelInfo.Name = lipgloss.NewStyle().Foreground(o.fgBase)
	s.ModelInfo.Provider = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.ModelInfo.ProviderFallback = lipgloss.NewStyle().Foreground(o.fgMoreSubtle).PaddingLeft(2)
	s.ModelInfo.Reasoning = lipgloss.NewStyle().Foreground(o.fgMostSubtle).PaddingLeft(2)
	s.ModelInfo.TokenCount = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.ModelInfo.TokenPercentage = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.ModelInfo.EstimatedUsagePrefix = s.ModelInfo.TokenPercentage
	s.ModelInfo.Cost = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
}
