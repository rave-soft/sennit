package styles

import "charm.land/lipgloss/v2"

// quickStyleStatus fills in Status (the bottom status bar), Completions
// (the "@"/"/" popup), Attachments (chips), and Pills (session-panel pills).
func quickStyleStatus(s *Styles, o quickStyleOpts, base, _, _ lipgloss.Style) {
	s.Status.Help = lipgloss.NewStyle().Padding(0, 1)
	// Notifications render as a quiet "icon message" line in place of the
	// help view (see Status.Draw) — no filled full-width bar. Only the
	// small leading icon carries the semantic color; the text itself
	// stays neutral.
	s.Status.SuccessIndicator = base.Foreground(o.successMostSubtle).PaddingLeft(1).SetString(ToolSuccess)
	s.Status.InfoIndicator = base.Foreground(o.info).PaddingLeft(1).SetString(ToolPending)
	s.Status.UpdateIndicator = s.Status.InfoIndicator
	s.Status.WarnIndicator = base.Foreground(o.warning).PaddingLeft(1).Bold(true).SetString("!")
	s.Status.ErrorIndicator = base.Foreground(o.error).PaddingLeft(1).Bold(true).SetString(RemoveIcon)
	s.Status.SuccessMessage = base.Foreground(o.fgSubtle).Padding(0, 1)
	s.Status.InfoMessage = s.Status.SuccessMessage
	s.Status.UpdateMessage = s.Status.SuccessMessage
	s.Status.WarnMessage = s.Status.SuccessMessage
	s.Status.ErrorMessage = base.Foreground(o.fgBase).Padding(0, 1)

	// Completions styles
	s.Completions.Normal = base.Background(o.bgLessVisible).Foreground(o.fgBase)
	s.Completions.Focused = base.Background(o.primary).Foreground(o.onPrimary)
	s.Completions.Match = base.Underline(true)
	s.Completions.Muted = base.Background(o.bgLessVisible).Foreground(o.fgSubtle)
	s.Completions.Border = base.Border(lipgloss.RoundedBorder()).
		BorderForeground(o.primary).
		BorderBackground(o.bgLessVisible).
		Background(o.bgLessVisible)

	// Attachments styles
	attachmentIconStyle := base.Foreground(o.bgLessVisible).Background(o.success).Padding(0, 1)
	s.Attachments.Image = attachmentIconStyle.SetString(ImageIcon)
	s.Attachments.Text = attachmentIconStyle.SetString(TextIcon)
	s.Attachments.Skill = attachmentIconStyle.SetString(SkillIcon)
	s.Attachments.Normal = base.Padding(0, 1).Background(o.fgMoreSubtle).Foreground(o.fgBase)
	// Remove and Deleting share the same slot on the right side of a chip
	// and must keep the same geometry so toggling delete-mode doesn't
	// shift the chips. Padding(0, 1) puts a colored cell on each side of the
	// glyph so it isn't flush against the box edge, while MarginRight(1)
	// keeps a transparent gap between adjacent chips.
	s.Attachments.Remove = base.Padding(0, 1).MarginRight(1).Background(o.bgLessVisible).Foreground(o.fgSubtle).SetString(RemoveIcon)
	s.Attachments.RemoveHover = base.Padding(0, 1).MarginRight(1).Background(o.bgHover).Foreground(o.fgBase).SetString(RemoveIcon)
	s.Attachments.Deleting = base.Padding(0, 1).MarginRight(1).Bold(true).Background(o.destructive).Foreground(o.fgBase)

	// Pills styles
	s.Pills.Base = base.Padding(0, 1)
	s.Pills.Focused = base.Padding(0, 1).BorderStyle(lipgloss.RoundedBorder()).BorderForeground(o.bgMostVisible)
	s.Pills.QueueItemPrefix = lipgloss.NewStyle().Foreground(o.fgMoreSubtle).SetString("  •")
	s.Pills.QueueItemText = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Pills.QueueLabel = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Pills.QueueIconBase = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Pills.QueueGradFromColor = o.error
	s.Pills.QueueGradToColor = o.secondary
	s.Pills.TodoLabel = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Pills.TodoProgress = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Pills.TodoCurrentTask = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Pills.TodoSpinner = lipgloss.NewStyle().Foreground(o.successMostSubtle)
	s.Pills.HelpKey = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Pills.HelpText = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Pills.Area = base
	s.Pills.HeaderHover = lipgloss.NewStyle().Background(o.bgMostVisible).Foreground(o.fgBase)
}
