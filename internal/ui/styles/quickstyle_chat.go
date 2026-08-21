package styles

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// quickStyleChat fills in Messages (chat message item styles), the ANSI
// 16-color remap used by shell output, and TextSelection.
func quickStyleChat(s *Styles, o quickStyleOpts, base, muted, subtle lipgloss.Style) {
	messageFocussedBorder := lipgloss.Border{
		Left: "▌",
	}

	s.Messages.NoContent = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Messages.UserBlurred = s.Messages.NoContent.PaddingLeft(1).BorderLeft(true).
		BorderForeground(o.primary).BorderStyle(lipgloss.NormalBorder())
	s.Messages.UserFocused = s.Messages.NoContent.PaddingLeft(1).BorderLeft(true).
		BorderForeground(o.primary).BorderStyle(messageFocussedBorder)
	s.Messages.AssistantBlurred = s.Messages.NoContent.PaddingLeft(2)
	s.Messages.AssistantFocused = s.Messages.NoContent.PaddingLeft(1).BorderLeft(true).
		BorderForeground(o.successMostSubtle).BorderStyle(messageFocussedBorder)
	s.Messages.Thinking = lipgloss.NewStyle().MaxHeight(10)
	s.Messages.ErrorTag = lipgloss.NewStyle().Padding(0, 1).
		Background(o.destructive).Foreground(o.onPrimary)
	s.Messages.OriginAgentTag = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Messages.Notice = lipgloss.NewStyle().Foreground(o.fgMostSubtle).Italic(true)
	s.Messages.ErrorTitle = lipgloss.NewStyle().Foreground(o.fgSubtle)
	s.Messages.ErrorDetails = lipgloss.NewStyle().Foreground(o.fgMostSubtle)

	// Message item styles
	s.Messages.ToolCallFocused = muted.PaddingLeft(1).
		BorderStyle(messageFocussedBorder).
		BorderLeft(true).
		BorderForeground(o.successMostSubtle)
	s.Messages.ToolCallBlurred = muted.PaddingLeft(2)
	// No padding or border for compact tool calls within messages
	s.Messages.ToolCallCompact = muted

	// ANSI 16-color palette (indices 0-7 normal, 8-15 bright). Used to
	// remap raw terminal color codes in command output onto legible
	// colors. See [Styles.ANSI].
	s.ANSI = [16]color.Color{
		o.ansiBlack, o.ansiRed, o.ansiGreen, o.ansiYellow,
		o.ansiBlue, o.ansiMagenta, o.ansiCyan, o.ansiWhite,
		o.ansiBrightBlack, o.ansiBrightRed, o.ansiBrightGreen, o.ansiBrightYellow,
		o.ansiBrightBlue, o.ansiBrightMagenta, o.ansiBrightCyan, o.ansiBrightWhite,
	}

	// Shell (bang mode) item styles.
	s.Messages.ShellBarFocused = lipgloss.NewStyle().PaddingLeft(1).
		BorderStyle(messageFocussedBorder).BorderLeft(true).
		BorderForeground(o.primary)
	s.Messages.ShellBarBlurred = lipgloss.NewStyle().PaddingLeft(1).BorderLeft(true).
		BorderForeground(o.bgMostVisible).BorderStyle(lipgloss.NormalBorder())
	s.Messages.ShellPrompt = base.Foreground(o.primary).Bold(true)
	s.Messages.ShellPromptBlurred = base.Foreground(o.fgMoreSubtle)
	s.Messages.ShellCommand = base.Foreground(o.fgBase)
	s.Messages.ShellOutput = lipgloss.NewStyle().Foreground(o.fgSubtle)
	s.Messages.ShellOutputHover = lipgloss.NewStyle().Foreground(o.fgSubtle).Background(o.bgHover)
	s.Messages.ShellExitCode = lipgloss.NewStyle().Foreground(o.destructive)
	s.Messages.ShellTruncation = muted
	s.Messages.ShellTruncationHover = muted.Background(o.bgHover)

	s.Messages.SectionHeader = base.PaddingLeft(2)
	s.Messages.ChatSeparator = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Messages.MarkdownBlock = lipgloss.NewStyle().Background(o.bgMarkdown)
	s.Messages.AssistantInfoIcon = subtle
	s.Messages.AssistantInfoModel = muted
	s.Messages.AssistantInfoProvider = subtle
	s.Messages.AssistantInfoDuration = subtle
	s.Messages.AssistantCanceled = lipgloss.NewStyle().Foreground(o.fgSubtle).Italic(true)

	// Thinking section styles
	s.Messages.ThinkingBox = subtle.Background(o.bgLeastVisible)
	s.Messages.ThinkingBoxHover = subtle.Background(o.bgHover)
	s.Messages.ThinkingTruncationHint = muted
	s.Messages.ThinkingFooterTitle = muted
	s.Messages.ThinkingFooterDuration = subtle

	// Text selection.
	s.TextSelection = lipgloss.NewStyle().Foreground(o.onPrimary).Background(o.primary)
}
