package styles

import "charm.land/lipgloss/v2"

// quickStyleTool fills in Tool, the styles used to render tool-call items
// in the chat (icons, names, params, content, diffs, jobs, hooks, todos, …).
func quickStyleTool(s *Styles, o quickStyleOpts, base, muted, subtle lipgloss.Style) {
	// Tool rendering styles
	s.Tool.IconPending = base.Foreground(o.successMostSubtle).SetString(ToolPending)
	s.Tool.IconSuccess = base.Foreground(o.success).SetString(ToolSuccess)
	s.Tool.IconError = base.Foreground(o.error).SetString(ToolError)
	s.Tool.IconCancelled = muted.SetString(ToolPending)

	s.Tool.NameNormal = base.Foreground(o.info)
	s.Tool.NameNested = base.Foreground(o.info)

	s.Tool.ParamMain = subtle
	s.Tool.ParamKey = subtle

	// Content rendering - prepared styles that accept width parameter
	s.Tool.ContentLine = base.Background(o.bgMarkdown)
	s.Tool.ContentTruncation = muted.Background(o.bgMarkdown)
	s.Tool.ContentTruncationHover = muted.Background(o.bgHover)
	s.Tool.ContentCodeLine = base.Background(o.bgBase).PaddingLeft(2)
	s.Tool.ContentCodeTruncation = muted.Background(o.bgBase).PaddingLeft(2)
	s.Tool.ContentCodeBg = o.bgBase
	s.Tool.Body = base.PaddingLeft(2)
	s.Tool.ClickableHoverBg = o.bgHover

	// Deprecated - kept for backward compatibility
	s.Tool.ContentBg = muted.Background(o.bgLeastVisible)
	s.Tool.ContentText = muted
	s.Tool.ContentLineNumber = base.Foreground(o.fgMoreSubtle).Background(o.bgBase).PaddingRight(1).PaddingLeft(1)

	s.Tool.StateWaiting = base.Foreground(o.fgMostSubtle)
	s.Tool.StateCancelled = base.Foreground(o.fgMostSubtle)

	s.Tool.ErrorTag = base.Padding(0, 1).Background(o.destructive).Foreground(o.onPrimary)
	s.Tool.ErrorMessage = base.Foreground(o.fgSubtle)

	s.Tool.WarnTag = base.Padding(0, 1).Background(o.attention).Foreground(o.bgBase).Bold(true)
	s.Tool.WarnMessage = base.Foreground(o.fgSubtle)

	// Diff and multi-edit styles
	s.Tool.DiffTruncation = muted.Background(o.bgMarkdown).PaddingLeft(2)
	s.Tool.DiffTruncationHover = muted.Background(o.bgHover).PaddingLeft(2)
	s.Tool.NoteTag = base.Padding(0, 1).Background(o.info).Foreground(o.onPrimary)
	s.Tool.NoteMessage = base.Foreground(o.fgSubtle)

	// Job header styles
	s.Tool.JobIconPending = base.Foreground(o.successMostSubtle)
	s.Tool.JobIconError = base.Foreground(o.error)
	s.Tool.JobIconSuccess = base.Foreground(o.success)
	s.Tool.JobToolName = base.Foreground(o.info)
	s.Tool.JobPID = muted
	s.Tool.JobDescription = subtle

	// Agent task styles
	s.Tool.AgentTaskTag = base.Bold(true).Padding(0, 1).MarginLeft(2).Background(o.infoMoreSubtle).Foreground(o.onPrimary)
	s.Tool.AgentPrompt = muted

	// Agentic fetch styles
	s.Tool.AgenticFetchPromptTag = base.Bold(true).Padding(0, 1).MarginLeft(2).Background(o.success).Foreground(o.separator)

	// Todo styles
	s.Tool.TodoRatio = base.Foreground(o.infoMostSubtle)
	s.Tool.TodoCompletedIcon = base.Foreground(o.success)
	s.Tool.TodoInProgressIcon = base.Foreground(o.successMostSubtle)
	s.Tool.TodoPendingIcon = base.Foreground(o.fgMoreSubtle)
	s.Tool.TodoStatusNote = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Tool.TodoItem = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Tool.TodoJustStarted = lipgloss.NewStyle().Foreground(o.fgBase)

	// MCP styles
	s.Tool.MCPName = base.Foreground(o.info)
	s.Tool.MCPToolName = base.Foreground(o.infoMostSubtle)
	s.Tool.MCPArrow = base.Foreground(o.info).SetString(ArrowRightIcon)

	// Loading indicators for images, skills
	s.Tool.ResourceLoadedText = base.Foreground(o.success)
	s.Tool.ResourceLoadedIndicator = base.Foreground(o.successMostSubtle)
	s.Tool.ResourceName = base
	s.Tool.MediaType = base
	s.Tool.ResourceSize = base.Foreground(o.fgMoreSubtle)

	// Hook styles
	s.Tool.HookLabel = base.Foreground(o.successMoreSubtle)
	s.Tool.HookName = base
	s.Tool.HookMatcher = base.Foreground(o.fgMoreSubtle)
	s.Tool.HookArrow = base.Foreground(o.successMoreSubtle)
	s.Tool.HookDetail = base.Foreground(o.fgMoreSubtle)
	s.Tool.HookOK = base.Foreground(o.successMostSubtle)
	s.Tool.HookDenied = base.Foreground(o.error)
	s.Tool.HookDeniedLabel = base.Foreground(o.destructive)
	s.Tool.HookDeniedReason = base.Foreground(o.bgMostVisible)
	s.Tool.HookRewrote = base.Foreground(o.bgMostVisible)

	// Tool-call action verbs and result-list styling.
	s.Tool.ActionCreate = lipgloss.NewStyle().Foreground(o.successMoreSubtle)
	s.Tool.ActionDestroy = lipgloss.NewStyle().Foreground(o.destructive)
	s.Tool.ResultEmpty = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Tool.ResultTruncation = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
	s.Tool.ResultItemName = lipgloss.NewStyle().Foreground(o.fgBase)
	s.Tool.ResultItemDesc = lipgloss.NewStyle().Foreground(o.fgMostSubtle)
}
