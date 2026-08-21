package styles

import "charm.land/lipgloss/v2"

// quickStyleDialog fills in Dialog, the overlay dialog chrome (title,
// list items, permissions, quit, arguments, sessions, …).
func quickStyleDialog(s *Styles, o quickStyleOpts, base, _, _ lipgloss.Style) {
	// Dialog styles
	s.Dialog.Title = base.Padding(0, 1).Foreground(o.primary)
	s.Dialog.TitleText = base.Foreground(o.primary)
	s.Dialog.TitleError = base.Foreground(o.destructive)
	s.Dialog.TitleAccent = base.Foreground(o.success).Bold(true)
	s.Dialog.TitleLineBase = lipgloss.NewStyle()
	s.Dialog.TitleGradFromColor = o.primary
	s.Dialog.TitleGradToColor = o.secondary

	// Dialog.ListItem (commands, reasoning, models). The info column holds
	// secondary hints like keybind shortcuts, so mute it when blurred and
	// keep it readable on the focused row.
	s.Dialog.ListItem.InfoBlurred = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Dialog.ListItem.InfoFocused = lipgloss.NewStyle().Foreground(o.fgBase)

	// Dialog.Permissions
	s.Dialog.Permissions.KeyText = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Dialog.Permissions.ValueText = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Dialog.Permissions.ParamsBg = o.bgLessVisible

	// Dialog.Quit
	s.Dialog.Quit.Content = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Dialog.Quit.Hint = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Dialog.Quit.Frame = lipgloss.NewStyle().BorderForeground(o.primary).Border(lipgloss.RoundedBorder()).Padding(1, 2)
	s.Dialog.View = base.Border(lipgloss.RoundedBorder()).BorderForeground(o.primary)
	s.Dialog.PrimaryText = base.Padding(0, 1).Foreground(o.primary)
	s.Dialog.SecondaryText = base.Padding(0, 1).Foreground(o.fgMostSubtle)
	s.Dialog.HelpView = base.Padding(0, 1).AlignHorizontal(lipgloss.Left)
	s.Dialog.Help.ShortKey = base.Foreground(o.fgMoreSubtle)
	s.Dialog.Help.ShortDesc = base.Foreground(o.fgMostSubtle)
	s.Dialog.Help.ShortSeparator = base.Foreground(o.separator)
	s.Dialog.Help.Ellipsis = base.Foreground(o.separator)
	s.Dialog.Help.FullKey = base.Foreground(o.fgMoreSubtle)
	s.Dialog.Help.FullDesc = base.Foreground(o.fgMostSubtle)
	s.Dialog.Help.FullSeparator = base.Foreground(o.separator)
	s.Dialog.NormalItem = base.Padding(0, 1).Foreground(o.fgBase)
	s.Dialog.SelectedItem = base.Padding(0, 1).Background(o.primary).Foreground(o.onPrimary)
	s.Dialog.InputPrompt = base.Margin(1, 1)

	s.Dialog.List = base.Margin(0, 0, 1, 0)
	s.Dialog.ContentPanel = base.Background(o.bgLessVisible).Foreground(o.fgBase).Padding(1, 2)
	s.Dialog.Spinner = base.Foreground(o.secondary)
	s.Dialog.ScrollbarThumb = base.Foreground(o.secondary)
	s.Dialog.ScrollbarTrack = base.Foreground(o.separator)
	s.Dialog.ScrollbarThumbHover = base.Foreground(o.primary).Bold(true)

	s.Dialog.ImagePreview = lipgloss.NewStyle().Padding(0, 1).Foreground(o.fgMostSubtle)

	// API key input dialog
	s.Dialog.APIKey.Spinner = base.Foreground(o.success)

	// OAuth dialog
	s.Dialog.OAuth.Spinner = base.Foreground(o.successMoreSubtle)
	s.Dialog.OAuth.Instructions = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Dialog.OAuth.UserCode = lipgloss.NewStyle().Bold(true).Foreground(o.fgBase)
	s.Dialog.OAuth.Success = lipgloss.NewStyle().Foreground(o.successMoreSubtle)
	s.Dialog.OAuth.Link = lipgloss.NewStyle().Foreground(o.successMostSubtle).Underline(true)
	s.Dialog.OAuth.Enter = lipgloss.NewStyle().Foreground(o.keyword)
	s.Dialog.OAuth.ErrorText = lipgloss.NewStyle().Foreground(o.error)
	s.Dialog.OAuth.StatusText = lipgloss.NewStyle().Foreground(o.fgMoreSubtle)
	s.Dialog.OAuth.UserCodeBg = o.bgLeastVisible

	s.Dialog.Arguments.Content = base.Padding(1)
	s.Dialog.Arguments.Description = base.MarginBottom(1).MaxHeight(3)
	s.Dialog.Arguments.InputLabelBlurred = base.Foreground(o.fgMoreSubtle)
	s.Dialog.Arguments.InputLabelFocused = base.Bold(true)
	s.Dialog.Arguments.InputRequiredMarkBlurred = base.Foreground(o.fgMoreSubtle).SetString("*")
	s.Dialog.Arguments.InputRequiredMarkFocused = base.Foreground(o.primary).Bold(true).SetString("*")

	s.Dialog.Sessions.DeletingTitle = s.Dialog.Title.Foreground(o.destructive)
	s.Dialog.Sessions.DeletingView = s.Dialog.View.BorderForeground(o.destructive)
	s.Dialog.Sessions.DeletingMessage = base.Padding(1)
	s.Dialog.Sessions.DeletingTitleGradientFromColor = o.destructive
	s.Dialog.Sessions.DeletingTitleGradientToColor = o.primary
	s.Dialog.Sessions.DeletingItemBlurred = s.Dialog.NormalItem.Foreground(o.fgMostSubtle)
	s.Dialog.Sessions.DeletingItemFocused = s.Dialog.SelectedItem.Background(o.destructive).Foreground(o.onPrimary)

	s.Dialog.Sessions.RenamingingTitle = s.Dialog.Title.Foreground(o.warningSubtle)
	s.Dialog.Sessions.RenamingView = s.Dialog.View.BorderForeground(o.warningSubtle)
	s.Dialog.Sessions.RenamingingMessage = base.Padding(1)
	s.Dialog.Sessions.RenamingTitleGradientFromColor = o.warningSubtle
	s.Dialog.Sessions.RenamingTitleGradientToColor = o.accent
	s.Dialog.Sessions.RenamingItemBlurred = s.Dialog.NormalItem.Foreground(o.fgMostSubtle)
	s.Dialog.Sessions.RenamingingItemFocused = s.Dialog.SelectedItem.UnsetBackground().UnsetForeground()
	s.Dialog.Sessions.RenamingPlaceholder = base.Foreground(o.fgMoreSubtle)
	s.Dialog.Sessions.InfoBlurred = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Dialog.Sessions.InfoFocused = lipgloss.NewStyle().Foreground(o.fgBase)
}
