package dialog

import (
	"encoding/json"
	"fmt"
	"image"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/stringext"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

// PermissionsID is the identifier for the permissions dialog.
const PermissionsID = "permissions"

// PermissionAction represents the user's response to a permission request.
type PermissionAction string

const (
	PermissionAllow           PermissionAction = "allow"
	PermissionAllowForSession PermissionAction = "allow_session"
	PermissionDeny            PermissionAction = "deny"
)

// Permissions dialog sizing constants.
const (
	// diffMaxWidth is the maximum width for diff views.
	diffMaxWidth = 180
	// diffSizeRatio is the size ratio for diff views relative to window.
	diffSizeRatio = 0.8
	// simpleMaxWidth is the maximum width for simple content dialogs.
	simpleMaxWidth = 100
	// simpleSizeRatio is the size ratio for simple content dialogs.
	simpleSizeRatio = 0.6
	// simpleHeightRatio is the height ratio for simple content dialogs.
	simpleHeightRatio = 0.5
	// splitModeMinWidth is the minimum width to enable split diff mode.
	splitModeMinWidth = 140
	// layoutSpacingLines is the number of empty lines used for layout spacing.
	layoutSpacingLines = 4
	// minWindowWidth is the minimum window width before forcing fullscreen.
	minWindowWidth = 77
	// minWindowHeight is the minimum window height before forcing fullscreen.
	minWindowHeight = 20
)

// Permissions represents a dialog for permission requests.
type Permissions struct {
	com        *common.Common
	fullscreen bool // true when dialog is fullscreen

	permission     permission.PermissionRequest
	selectedOption int // 0: Allow, 1: Allow for session, 2: Deny

	viewport      viewport.Model
	viewportDirty bool // true when viewport content needs to be re-rendered

	// Diff view state.
	diffSplitMode        *bool // nil means use default based on width
	defaultDiffSplitMode bool  // default split mode based on width
	diffXOffset          int   // horizontal scroll offset for diff view
	unifiedDiffContent   string
	splitDiffContent     string

	help   help.Model
	keyMap permissionsKeyMap

	// buttonRects holds the absolute screen rectangles of the Allow /
	// Allow for Session / Deny buttons from the most recent Draw, used
	// to hit-test mouse clicks and hover (mirrors the childPanelButtonRect
	// pattern in model/child_session_panel.go: geometry is captured at
	// draw time and read back on the next mouse event).
	buttonRects [3]image.Rectangle
	// hoverIndex is the index of the button currently under the pointer,
	// or -1 when the pointer isn't over any button.
	hoverIndex int
}

type permissionsKeyMap struct {
	Left             key.Binding
	Right            key.Binding
	Tab              key.Binding
	Select           key.Binding
	Allow            key.Binding
	AllowSession     key.Binding
	Deny             key.Binding
	Close            key.Binding
	ToggleDiffMode   key.Binding
	ToggleFullscreen key.Binding
	ScrollUp         key.Binding
	ScrollDown       key.Binding
	ScrollLeft       key.Binding
	ScrollRight      key.Binding
	Choose           key.Binding
	Scroll           key.Binding
}

func defaultPermissionsKeyMap() permissionsKeyMap {
	return permissionsKeyMap{
		Left: key.NewBinding(
			key.WithKeys("left", "h"),
			key.WithHelp("←", "previous"),
		),
		Right: key.NewBinding(
			key.WithKeys("right", "l"),
			key.WithHelp("→", "next"),
		),
		Tab: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "next option"),
		),
		Select: key.NewBinding(
			key.WithKeys("enter", "ctrl+y"),
			key.WithHelp("enter", "confirm"),
		),
		Allow: key.NewBinding(
			key.WithKeys("a", "A", "ctrl+a"),
			key.WithHelp("a", "allow"),
		),
		AllowSession: key.NewBinding(
			key.WithKeys("s", "S", "ctrl+s"),
			key.WithHelp("s", "allow session"),
		),
		Deny: key.NewBinding(
			key.WithKeys("d", "D"),
			key.WithHelp("d", "deny"),
		),
		Close: CloseKey,
		ToggleDiffMode: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "toggle diff view"),
		),
		ToggleFullscreen: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "toggle fullscreen"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("shift+up", "K"),
			key.WithHelp("shift+↑", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("shift+down", "J"),
			key.WithHelp("shift+↓", "scroll down"),
		),
		ScrollLeft: key.NewBinding(
			key.WithKeys("shift+left", "H"),
			key.WithHelp("shift+←", "scroll left"),
		),
		ScrollRight: key.NewBinding(
			key.WithKeys("shift+right", "L"),
			key.WithHelp("shift+→", "scroll right"),
		),
		Choose: key.NewBinding(
			key.WithKeys("left", "right"),
			key.WithHelp("←/→", "choose"),
		),
		Scroll: key.NewBinding(
			key.WithKeys("shift+left", "shift+down", "shift+up", "shift+right"),
			key.WithHelp("shift+←↓↑→", "scroll"),
		),
	}
}

var _ Dialog = (*Permissions)(nil)

// PermissionsOption configures the permissions dialog.
type PermissionsOption func(*Permissions)

// WithDiffMode sets the initial diff mode (split or unified).
func WithDiffMode(split bool) PermissionsOption {
	return func(p *Permissions) {
		p.diffSplitMode = &split
	}
}

// NewPermissions creates a new permissions dialog.
func NewPermissions(com *common.Common, perm permission.PermissionRequest, opts ...PermissionsOption) *Permissions {
	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()

	km := defaultPermissionsKeyMap()

	// Configure viewport with matching keybindings.
	vp := viewport.New()
	vp.KeyMap = viewport.KeyMap{
		Up:    km.ScrollUp,
		Down:  km.ScrollDown,
		Left:  km.ScrollLeft,
		Right: km.ScrollRight,
		// Disable other viewport keys to avoid conflicts with dialog shortcuts.
		PageUp:       key.NewBinding(key.WithDisabled()),
		PageDown:     key.NewBinding(key.WithDisabled()),
		HalfPageUp:   key.NewBinding(key.WithDisabled()),
		HalfPageDown: key.NewBinding(key.WithDisabled()),
	}

	p := &Permissions{
		com:            com,
		permission:     perm,
		selectedOption: 0,
		viewport:       vp,
		help:           h,
		keyMap:         km,
		hoverIndex:     -1,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Calculate usable content width (dialog border + horizontal padding).
func (p *Permissions) calculateContentWidth(width int) int {
	t := p.com.Styles
	const dialogHorizontalPadding = 2
	return width - t.Dialog.View.GetHorizontalFrameSize() - dialogHorizontalPadding
}

// ID implements [Dialog].
func (*Permissions) ID() string {
	return PermissionsID
}

// ToolCallID returns the tool call ID associated with this dialog's
// permission request.
func (p *Permissions) ToolCallID() string {
	return p.permission.ToolCallID
}

// HandleMsg implements [Dialog].
func (p *Permissions) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, p.keyMap.Close):
			// Escape denies the permission request.
			return p.respond(PermissionDeny)
		case key.Matches(msg, p.keyMap.Right), key.Matches(msg, p.keyMap.Tab):
			p.selectedOption = (p.selectedOption + 1) % 3
		case key.Matches(msg, p.keyMap.Left):
			// Add 2 instead of subtracting 1 to avoid negative modulo.
			p.selectedOption = (p.selectedOption + 2) % 3
		case key.Matches(msg, p.keyMap.Select):
			return p.selectCurrentOption()
		case key.Matches(msg, p.keyMap.Allow):
			return p.respond(PermissionAllow)
		case key.Matches(msg, p.keyMap.AllowSession):
			return p.respond(PermissionAllowForSession)
		case key.Matches(msg, p.keyMap.Deny):
			return p.respond(PermissionDeny)
		case key.Matches(msg, p.keyMap.ToggleDiffMode):
			if p.hasDiffView() {
				newMode := !p.isSplitMode()
				p.diffSplitMode = &newMode
				p.viewportDirty = true
			}
		case key.Matches(msg, p.keyMap.ToggleFullscreen):
			if p.hasDiffView() {
				p.fullscreen = !p.fullscreen
			}
		case key.Matches(msg, p.keyMap.ScrollDown):
			p.viewport, _ = p.viewport.Update(msg)
		case key.Matches(msg, p.keyMap.ScrollUp):
			p.viewport, _ = p.viewport.Update(msg)
		case key.Matches(msg, p.keyMap.ScrollLeft):
			if p.hasDiffView() {
				p.scrollLeft()
			} else {
				p.viewport, _ = p.viewport.Update(msg)
			}
		case key.Matches(msg, p.keyMap.ScrollRight):
			if p.hasDiffView() {
				p.scrollRight()
			} else {
				p.viewport, _ = p.viewport.Update(msg)
			}
		}
	case common.CoalescedWheelMsg:
		if p.hasDiffView() {
			if msg.DeltaX < 0 {
				p.scrollLeft()
			} else if msg.DeltaX > 0 {
				p.scrollRight()
			} else {
				p.viewport, _ = p.viewport.Update(tea.MouseWheelMsg(msg.Mouse))
			}
		} else {
			p.viewport, _ = p.viewport.Update(tea.MouseWheelMsg(msg.Mouse))
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return nil
		}
		if idx, ok := p.hitButton(msg.X, msg.Y); ok {
			p.selectedOption = idx
			p.hoverIndex = idx
			return p.selectCurrentOption()
		}
	case tea.MouseMotionMsg:
		// Hover moves the keyboard selection too, so the highlighted
		// button always matches what a subsequent click or enter would
		// pick.
		if idx, ok := p.hitButton(msg.X, msg.Y); ok {
			p.hoverIndex = idx
			p.selectedOption = idx
		} else {
			p.hoverIndex = -1
		}
	default:
		// Pass unhandled keys to viewport for non-diff content scrolling.
		if !p.hasDiffView() {
			p.viewport, _ = p.viewport.Update(msg)
			p.viewportDirty = true
		}
	}

	return nil
}

func (p *Permissions) selectCurrentOption() tea.Msg {
	switch p.selectedOption {
	case 0:
		return p.respond(PermissionAllow)
	case 1:
		return p.respond(PermissionAllowForSession)
	default:
		return p.respond(PermissionDeny)
	}
}

func (p *Permissions) respond(action PermissionAction) tea.Msg {
	return ActionPermissionResponse{
		Permission: p.permission,
		Action:     action,
	}
}

// hitButton returns the index of the button whose rect (from the most
// recent Draw) contains the given absolute screen coordinates.
func (p *Permissions) hitButton(x, y int) (int, bool) {
	pt := image.Pt(x, y)
	for i, r := range p.buttonRects {
		if pt.In(r) {
			return i, true
		}
	}
	return 0, false
}

func (p *Permissions) hasDiffView() bool {
	switch p.permission.ToolName {
	case proto.EditToolName, proto.WriteToolName, proto.MultiEditToolName, proto.ReplaceSymbolToolName:
		return true
	}
	return false
}

func (p *Permissions) isSplitMode() bool {
	if p.diffSplitMode != nil {
		return *p.diffSplitMode
	}
	return p.defaultDiffSplitMode
}

const horizontalScrollStep = 5

func (p *Permissions) scrollLeft() {
	p.diffXOffset = max(0, p.diffXOffset-horizontalScrollStep)
	p.viewportDirty = true
}

func (p *Permissions) scrollRight() {
	p.diffXOffset += horizontalScrollStep
	p.viewportDirty = true
}

// Draw implements [Dialog].
// dialogSize returns the outer dialog width and maximum height for the
// given screen area, and whether the window is too small so the dialog
// must fill it. Diff views get more room than simple prompts.
func (p *Permissions) dialogSize(area uv.Rectangle) (width, maxHeight int, forceFullscreen bool) {
	forceFullscreen = area.Dx() <= minWindowWidth || area.Dy() <= minWindowHeight
	switch {
	case forceFullscreen || (p.fullscreen && p.hasDiffView()):
		width, maxHeight = area.Dx(), area.Dy()
	case p.hasDiffView():
		// Wide for side-by-side diffs, capped for readability.
		width = min(int(float64(area.Dx())*diffSizeRatio), diffMaxWidth)
		maxHeight = int(float64(area.Dy()) * diffSizeRatio)
	default:
		// Narrower for simple content like commands and URLs.
		width = min(int(float64(area.Dx())*simpleSizeRatio), simpleMaxWidth)
		maxHeight = int(float64(area.Dy()) * simpleHeightRatio)
	}
	return width, maxHeight, forceFullscreen
}

// contentViewportHeight returns the height for the scrollable content
// viewport given the height taken by the fixed chrome (fixedHeight) and
// the content's natural height. Simple prompts shrink to fit their
// content; diff and fullscreen views always take all remaining height.
func (p *Permissions) contentViewportHeight(forceFullscreen bool, maxHeight, fixedHeight, contentHeight int) int {
	if p.hasDiffView() || forceFullscreen {
		return maxHeight - fixedHeight
	}
	if fixedHeight+contentHeight < maxHeight {
		return max(contentHeight, 3)
	}
	return max(maxHeight-fixedHeight, 3)
}

func (p *Permissions) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := p.com.Styles
	width, maxHeight, forceFullscreen := p.dialogSize(area)
	dialogStyle := t.Dialog.View.Width(width).Padding(0, 1)
	// The dialog fills the screen when forced small or when a diff is
	// expanded; center the buttons then instead of hugging the far edge.
	fullscreen := forceFullscreen || (p.fullscreen && p.hasDiffView())

	contentWidth := p.calculateContentWidth(width)
	header := p.renderHeader(contentWidth)
	buttons := p.renderButtons(contentWidth, fullscreen)
	// Pack the hints to the content width so they truncate cleanly instead
	// of overflowing. The dialog frame supplies the padding, so this renders
	// the hint line without the extra help view inset that renderDialogHelp
	// applies for RenderContext dialogs.
	helpView := shortHelpLine(&p.help, p.ShortHelp(), contentWidth)

	// isSplitMode() is also read from the ToggleDiffMode key handler in
	// HandleMsg, which — unlike Draw — never learns the dialog's current
	// width, so the width-derived default has to be cached on the struct
	// for it to read back. Draw is the only place that width is known,
	// so this assignment cannot move there; it is idempotent for an
	// unchanged width, the same lazy-cache shape as buttonRects below.
	p.defaultDiffSplitMode = width >= splitModeMinWidth

	// A diff always reserves the scrollbar column below (see
	// contentViewportHeight), so its final width is known without
	// rendering anything first. Go straight to it instead of pre-rendering
	// at contentWidth only to discard that render a few lines down — for
	// diffs, that discarded pre-render would cost a full reformat every
	// dirty frame for a value contentViewportHeight never even reads.
	var renderedContent string
	var contentHeight int
	viewportWidth := contentWidth
	if p.hasDiffView() {
		viewportWidth = contentWidth - 1
		if p.viewport.Width() != viewportWidth {
			p.viewportDirty = true
		}
		renderedContent = p.renderContent(viewportWidth)
		contentHeight = lipgloss.Height(renderedContent)
	} else {
		// Non-diff content's need for a scrollbar depends on its
		// rendered height, so measure it at the full width first.
		renderedContent = p.renderContent(contentWidth)
		contentHeight = lipgloss.Height(renderedContent)
	}
	fixedHeight := lipgloss.Height(header) + lipgloss.Height(buttons) +
		lipgloss.Height(helpView) + dialogStyle.GetVerticalFrameSize() + layoutSpacingLines
	availableHeight := p.contentViewportHeight(forceFullscreen, maxHeight, fixedHeight, contentHeight)

	// Determine if scrollbar is needed.
	needsScrollbar := p.hasDiffView() || contentHeight > availableHeight
	if !p.hasDiffView() {
		if needsScrollbar {
			viewportWidth = contentWidth - 1 // Reserve space for scrollbar.
		}

		// The pre-render above measured at contentWidth; the scrollbar then
		// takes a column. Re-render at the viewport's actual width whenever
		// the two differ and this content is going to be installed — the
		// width-changed check alone missed the case that bites: toggling the
		// diff split marks the content dirty without changing the viewport's
		// width, so the wider render was set into the narrower viewport and
		// every line overflowed by one column.
		if p.viewport.Width() != viewportWidth {
			// Mark content as dirty if width has changed.
			p.viewportDirty = true
			renderedContent = p.renderContent(viewportWidth)
		} else if p.viewportDirty && viewportWidth != contentWidth {
			renderedContent = p.renderContent(viewportWidth)
		}
	}

	var content string
	p.viewport.SetWidth(viewportWidth)
	p.viewport.SetHeight(availableHeight)
	if p.viewportDirty {
		p.viewport.SetContent(renderedContent)
		p.viewportDirty = false
	}
	content = p.viewport.View()
	if needsScrollbar {
		content = joinScrollbar(t, content, availableHeight, p.viewport.TotalLineCount(), availableHeight, p.viewport.YOffset())
	}

	parts := []string{header}
	if content != "" {
		parts = append(parts, "", content)
	}
	parts = append(parts, "", buttons, "", helpView)

	innerContent := lipgloss.JoinVertical(lipgloss.Left, parts...)
	view := dialogStyle.Render(innerContent)
	DrawCenterCursor(scr, area, view, nil)

	// Recompute the same centered placement DrawCenterCursor just used, so
	// the button row's position within innerContent can be translated into
	// absolute screen coordinates matching incoming mouse messages (see
	// buttonRects/hitButton).
	outerW, outerH := lipgloss.Size(view)
	outerW = min(outerW, area.Dx())
	outerH = min(outerH, area.Dy())
	center := common.CenterRect(area, outerW, outerH)

	buttonsLine := lipgloss.Height(header)
	if content != "" {
		buttonsLine += 1 + lipgloss.Height(content)
	}
	buttonsLine++ // blank line separating content (or header) from buttons
	originX := center.Min.X + dialogStyle.GetBorderLeftSize() + dialogStyle.GetPaddingLeft()
	originY := center.Min.Y + dialogStyle.GetBorderTopSize() + dialogStyle.GetPaddingTop() + buttonsLine
	p.buttonRects = p.computeButtonRects(p.buttonOptsList(), contentWidth, fullscreen, originX, originY)

	return nil
}

func (p *Permissions) renderHeader(contentWidth int) string {
	t := p.com.Styles

	title := common.DialogTitle(t, "Permission Required", contentWidth-t.Dialog.Title.GetHorizontalFrameSize(), t.Dialog.TitleGradFromColor, t.Dialog.TitleGradToColor)
	title = t.Dialog.Title.Render(title)

	// Tool info.
	toolLine := p.renderToolName(contentWidth)

	lines := []string{title, "", toolLine}

	// Say who is asking whenever it is not the visible turn. Several
	// delegations can be running at once, each in its own worktree, so
	// without this the user is approving a command with no idea which
	// piece of work wants it — or against which checkout it will run.
	if ref := p.permission.Delegation; ref.ID != "" {
		lines = append(lines, p.renderKeyValue(delegationHeaderKey(ref.Kind), delegationHeaderValue(ref), contentWidth))
	}

	// Show generic Path only for tools that don't render their own file/path line.
	switch p.permission.ToolName {
	case proto.EditToolName, proto.WriteToolName, proto.MultiEditToolName,
		proto.ReadToolName, proto.ReplaceSymbolToolName,
		proto.DownloadToolName, proto.LSToolName:
		// These tools show their own File/Directory line below.
	default:
		lines = append(lines, p.renderKeyValue("Path", fsext.PrettyPath(p.permission.Path), contentWidth))
	}

	// Add tool-specific header info.
	switch p.permission.ToolName {
	case proto.BashToolName:
		if params, ok := p.permission.Params.(proto.BashPermissionsParams); ok {
			lines = append(lines, p.renderKeyValue("Desc", params.Description, contentWidth))
		}
	case proto.DownloadToolName:
		if params, ok := p.permission.Params.(proto.DownloadPermissionsParams); ok {
			lines = append(lines, p.renderKeyValue("URL", params.URL, contentWidth))
			lines = append(lines, p.renderKeyValue("File", fsext.PrettyPath(params.FilePath), contentWidth))
		}
	case proto.EditToolName, proto.WriteToolName, proto.MultiEditToolName, proto.ReadToolName, proto.ReplaceSymbolToolName:
		var filePath string
		switch params := p.permission.Params.(type) {
		case proto.EditPermissionsParams:
			filePath = params.FilePath
		case proto.WritePermissionsParams:
			filePath = params.FilePath
		case proto.MultiEditPermissionsParams:
			filePath = params.FilePath
		case proto.ReadPermissionsParams:
			filePath = params.FilePath
		case proto.ReplaceSymbolPermissionsParams:
			filePath = params.FilePath
		}
		if filePath != "" {
			lines = append(lines, p.renderKeyValue("File", fsext.PrettyPath(filePath), contentWidth))
		}
	case proto.LSToolName:
		if params, ok := p.permission.Params.(proto.LSPermissionsParams); ok {
			lines = append(lines, p.renderKeyValue("Directory", fsext.PrettyPath(params.Path), contentWidth))
		}
	}

	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}

func (p *Permissions) renderKeyValue(key, value string, width int) string {
	t := p.com.Styles
	keyStyle := t.Dialog.Permissions.KeyText
	valueStyle := t.Dialog.Permissions.ValueText

	keyStr := keyStyle.Render(key)
	valueStr := valueStyle.Width(width - lipgloss.Width(keyStr) - 1).Render(" " + value)

	return lipgloss.JoinHorizontal(lipgloss.Left, keyStr, valueStr)
}

func (p *Permissions) renderToolName(width int) string {
	toolName := p.permission.ToolName

	// Check if this is an MCP tool (format: mcp_<mcpname>_<toolname>). The
	// naive "first two underscores" split this used to do breaks the
	// moment a server's own config key contains an underscore
	// ("my_server" + "do_thing" used to render as "My → Server Do
	// Thing"), so resolve the boundary against the configured server
	// names when they're available; see [proto.SplitMCPToolName] for the
	// fallback when they're not (config unset, or the session predates a
	// server rename/removal).
	if mcpName, toolPart, ok := proto.SplitMCPToolName(toolName, p.knownMCPServerNames()); ok {
		toolName = fmt.Sprintf("%s %s %s", prettyName(mcpName), styles.ArrowRightIcon, prettyName(toolPart))
	}

	return p.renderKeyValue("Tool", toolName, width)
}

// knownMCPServerNames returns the MCP server names from the active config,
// used to resolve an MCP tool's "mcp_<server>_<tool>" name unambiguously
// (see renderToolName). Returns nil when no config is wired up (e.g. a
// dialog built directly in a test), which is a valid input to
// [proto.SplitMCPToolName] — it just falls back to the naive split.
func (p *Permissions) knownMCPServerNames() []string {
	// Config() panics on a zero-value Common (Workspace unset) — tests
	// build the dialog directly without one, so guard the same way
	// Context() already does for Ctx.
	if p.com == nil || p.com.Workspace == nil {
		return nil
	}
	cfg := p.com.Config()
	if cfg == nil {
		return nil
	}
	names := make([]string, 0, len(cfg.MCP))
	for name := range cfg.MCP {
		names = append(names, name)
	}
	return names
}

// prettyName converts snake_case or kebab-case to Title Case.
func prettyName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")
	return stringext.Capitalize(name)
}

// contentRenderer draws one tool's permission body (registry idiom follows internal/ui/chat/tools_registry.go).
type contentRenderer func(p *Permissions, width int) string

// contentRenderers maps a tool name to its renderer; an unregistered name falls back to renderDefaultContent.
var contentRenderers = map[string]contentRenderer{}

func registerContentRenderer(toolName string, r contentRenderer) {
	if _, exists := contentRenderers[toolName]; exists {
		panic("dialog: duplicate content renderer registration for " + toolName)
	}
	contentRenderers[toolName] = r
}

func init() {
	registerContentRenderer(proto.BashToolName, (*Permissions).renderBashContent)
	registerContentRenderer(proto.EditToolName, diffContentRenderer(func(p proto.EditPermissionsParams) diffParams {
		return diffParams{p.FilePath, p.OldContent, p.NewContent}
	}))
	registerContentRenderer(proto.WriteToolName, diffContentRenderer(func(p proto.WritePermissionsParams) diffParams {
		return diffParams{p.FilePath, p.OldContent, p.NewContent}
	}))
	registerContentRenderer(proto.MultiEditToolName, diffContentRenderer(func(p proto.MultiEditPermissionsParams) diffParams {
		return diffParams{p.FilePath, p.OldContent, p.NewContent}
	}))
	registerContentRenderer(proto.ReplaceSymbolToolName, diffContentRenderer(func(p proto.ReplaceSymbolPermissionsParams) diffParams {
		return diffParams{p.FilePath, p.OldContent, p.NewContent}
	}))
	registerContentRenderer(proto.DownloadToolName, (*Permissions).renderDownloadContent)
	registerContentRenderer(proto.FetchToolName, (*Permissions).renderFetchContent)
	registerContentRenderer(proto.AgenticFetchToolName, (*Permissions).renderAgenticFetchContent)
	registerContentRenderer(proto.ReadToolName, (*Permissions).renderViewContent)
	registerContentRenderer(proto.LSToolName, (*Permissions).renderLSContent)
}

func (p *Permissions) renderContent(width int) string {
	if r, ok := contentRenderers[p.permission.ToolName]; ok {
		return r(p, width)
	}
	return p.renderDefaultContent(width)
}

func (p *Permissions) renderBashContent(width int) string {
	params, ok := p.permission.Params.(proto.BashPermissionsParams)
	if !ok {
		return ""
	}

	return p.renderContentPanel(params.Command, width)
}

// diffParams is what renderDiff needs from a diff-view tool's params. The
// four concrete tools.*PermissionsParams types stay distinct per AGENTS.md
// (consumers assert on them after a JSON round trip); diffContentRenderer asserts each one before adapting it here.
type diffParams struct{ filePath, oldContent, newContent string }

// diffContentRenderer shares one contentRenderer across all four diff-view tools: assert Params to P, adapt via toDiff, else render nothing.
func diffContentRenderer[P any](toDiff func(P) diffParams) contentRenderer {
	return func(p *Permissions, width int) string {
		params, ok := p.permission.Params.(P)
		if !ok {
			return ""
		}
		dp := toDiff(params)
		return p.renderDiff(dp.filePath, dp.oldContent, dp.newContent, width)
	}
}

func (p *Permissions) renderDiff(filePath, oldContent, newContent string, contentWidth int) string {
	if !p.viewportDirty {
		if p.isSplitMode() {
			return p.splitDiffContent
		}
		return p.unifiedDiffContent
	}

	isSplitMode := p.isSplitMode()
	// Wrapping and horizontal scroll are mutually exclusive views of the
	// same content: with wrapping on, every column is already on screen,
	// so there is nothing off-screen for XOffset to reveal (DiffView
	// ignores XOffset while WrapLines is on — see diffview.go). Wrap only
	// while unscrolled; the first right-scroll switches to the
	// non-wrapped renderer, which does honour XOffset, and scrolling back
	// to 0 restores wrapping.
	formatter := common.DiffFormatter(p.com.Styles).
		Before(fsext.PrettyPath(filePath), oldContent).
		After(fsext.PrettyPath(filePath), newContent).
		XOffset(p.diffXOffset).
		Width(contentWidth).
		WrapLines(p.diffXOffset == 0)

	var result string
	if isSplitMode {
		formatter = formatter.Split()
		p.splitDiffContent = formatter.String()
		result = p.splitDiffContent
	} else {
		formatter = formatter.Unified()
		p.unifiedDiffContent = formatter.String()
		result = p.unifiedDiffContent
	}

	return result
}

func (p *Permissions) renderDownloadContent(width int) string {
	params, ok := p.permission.Params.(proto.DownloadPermissionsParams)
	if !ok {
		return ""
	}

	content := fmt.Sprintf("URL: %s\nFile: %s", params.URL, fsext.PrettyPath(params.FilePath))
	if params.Timeout > 0 {
		content += fmt.Sprintf("\nTimeout: %ds", params.Timeout)
	}

	return p.renderContentPanel(content, width)
}

func (p *Permissions) renderFetchContent(width int) string {
	params, ok := p.permission.Params.(proto.FetchPermissionsParams)
	if !ok {
		return ""
	}

	return p.renderContentPanel(params.URL, width)
}

func (p *Permissions) renderAgenticFetchContent(width int) string {
	params, ok := p.permission.Params.(proto.AgenticFetchPermissionsParams)
	if !ok {
		return ""
	}

	var content string
	if params.URL != "" {
		content = fmt.Sprintf("URL: %s\n\nPrompt: %s", params.URL, params.Prompt)
	} else {
		content = fmt.Sprintf("Prompt: %s", params.Prompt)
	}

	return p.renderContentPanel(content, width)
}

func (p *Permissions) renderViewContent(width int) string {
	params, ok := p.permission.Params.(proto.ReadPermissionsParams)
	if !ok {
		return ""
	}

	content := fmt.Sprintf("File: %s", fsext.PrettyPath(params.FilePath))
	if params.Offset > 0 {
		content += fmt.Sprintf("\nStarting from line: %d", params.Offset+1)
	}
	if params.Limit > 0 && params.Limit != 2000 {
		content += fmt.Sprintf("\nLines to read: %d", params.Limit)
	}

	return p.renderContentPanel(content, width)
}

func (p *Permissions) renderLSContent(width int) string {
	params, ok := p.permission.Params.(proto.LSPermissionsParams)
	if !ok {
		return ""
	}

	content := fmt.Sprintf("Directory: %s", fsext.PrettyPath(params.Path))
	if len(params.Ignore) > 0 {
		content += fmt.Sprintf("\nIgnore patterns: %s", strings.Join(params.Ignore, ", "))
	}

	return p.renderContentPanel(content, width)
}

func (p *Permissions) renderDefaultContent(width int) string {
	t := p.com.Styles
	var content string
	// do not add the description for mcp tools
	if !strings.HasPrefix(p.permission.ToolName, "mcp_") {
		content = p.permission.Description
	}

	// Pretty-print JSON params if available.
	if p.permission.Params != nil {
		// A struct rendered with %v comes out as Go's own dump —
		// {map[] 0xc000...} — which tells the person nothing about what
		// they are being asked to approve. Marshal it instead, and keep
		// %v only as the last resort for something that will not encode.
		var paramStr string
		switch v := p.permission.Params.(type) {
		case string:
			paramStr = v
		default:
			if b, err := json.Marshal(v); err == nil {
				paramStr = string(b)
			} else {
				paramStr = fmt.Sprintf("%v", v)
			}
		}

		var parsed any
		if err := json.Unmarshal([]byte(paramStr), &parsed); err == nil {
			if b, err := json.MarshalIndent(parsed, "", "  "); err == nil {
				jsonContent := string(b)
				highlighted, err := common.SyntaxHighlight(t, jsonContent, "params.json", t.Dialog.Permissions.ParamsBg)
				if err == nil {
					jsonContent = highlighted
				}
				if content != "" {
					content += "\n\n"
				}
				content += jsonContent
			}
		} else if paramStr != "" {
			if content != "" {
				content += "\n\n"
			}
			content += paramStr
		}
	}

	if content == "" {
		return ""
	}

	return p.renderContentPanel(strings.TrimSpace(content), width)
}

// renderContentPanel renders content in a panel with the full width.
func (p *Permissions) renderContentPanel(content string, width int) string {
	panelStyle := p.com.Styles.Dialog.ContentPanel
	return panelStyle.Width(width).Render(content)
}

// buttonOptsList builds the Allow / Allow for Session / Deny button specs
// from current selection and hover state. Shared by renderButtons (drawing)
// and buttonRects (click/hover hit-testing) so their button order and text
// never drift apart.
func (p *Permissions) buttonOptsList() []common.ButtonOpts {
	return []common.ButtonOpts{
		{Text: "Allow", UnderlineIndex: 0, Selected: p.selectedOption == 0, Hovered: p.hoverIndex == 0},
		{Text: "Allow for Session", UnderlineIndex: 10, Selected: p.selectedOption == 1, Hovered: p.hoverIndex == 1},
		{Text: "Deny", UnderlineIndex: 0, Selected: p.selectedOption == 2, Hovered: p.hoverIndex == 2},
	}
}

// buttonLayout decides whether the buttons render as a single horizontal
// row or, when they don't fit contentWidth, stack vertically, and which
// alignment to use. Shared between renderButtons and buttonRects so the
// rendered layout and the hit-test geometry always agree.
func (p *Permissions) buttonLayout(opts []common.ButtonOpts, contentWidth int, fullscreen bool) (vertical bool, align lipgloss.Position) {
	// Center when stacked or when the dialog fills the screen; otherwise
	// hug the right edge next to the content. Right-aligning across a
	// full-screen width leaves the buttons threaded in the corner.
	align = lipgloss.Right
	if fullscreen {
		align = lipgloss.Center
	}
	if lipgloss.Width(common.ButtonGroup(p.com.Styles, opts, "  ")) > contentWidth {
		return true, lipgloss.Center
	}
	return false, align
}

func (p *Permissions) renderButtons(contentWidth int, fullscreen bool) string {
	opts := p.buttonOptsList()
	vertical, align := p.buttonLayout(opts, contentWidth, fullscreen)

	spacing := "  "
	if vertical {
		spacing = "\n"
	}

	return lipgloss.NewStyle().
		Width(contentWidth).
		Align(align).
		Render(common.ButtonGroup(p.com.Styles, opts, spacing))
}

// computeButtonRects computes the absolute on-screen rectangle of each
// button (Allow, Allow for Session, Deny), given the row's origin
// (top-left of its content-width slot, in absolute screen coordinates)
// and the same layout decision renderButtons made. Button style
// (selected/hovered) doesn't affect width, so opts here need not match
// hover state exactly.
func (p *Permissions) computeButtonRects(opts []common.ButtonOpts, contentWidth int, fullscreen bool, originX, originY int) [3]image.Rectangle {
	var rects [3]image.Rectangle
	vertical, align := p.buttonLayout(opts, contentWidth, fullscreen)

	widths := make([]int, len(opts))
	for i, o := range opts {
		widths[i] = lipgloss.Width(common.Button(p.com.Styles, o))
	}

	if vertical {
		y := originY
		for i, w := range widths {
			x := originX + max(0, contentWidth-w)/2
			rects[i] = image.Rect(x, y, x+w, y+1)
			y++
		}
		return rects
	}

	const sepWidth = 2 // matches the "  " spacing passed to ButtonGroup.
	total := 0
	for i, w := range widths {
		total += w
		if i > 0 {
			total += sepWidth
		}
	}
	var pad int
	switch align {
	case lipgloss.Right:
		pad = max(0, contentWidth-total)
	case lipgloss.Center:
		pad = max(0, contentWidth-total) / 2
	}

	x := originX + pad
	for i, w := range widths {
		rects[i] = image.Rect(x, originY, x+w, originY+1)
		x += w + sepWidth
	}
	return rects
}

func (p *Permissions) canScroll() bool {
	if p.hasDiffView() {
		// Diff views can always scroll.
		return true
	}
	// For non-diff content, check if viewport has scrollable content.
	return !p.viewport.AtTop() || !p.viewport.AtBottom()
}

// ShortHelp implements [help.KeyMap].
func (p *Permissions) ShortHelp() []key.Binding {
	bindings := []key.Binding{
		p.keyMap.Choose,
		p.keyMap.Select,
		p.keyMap.Close,
	}

	if p.canScroll() {
		bindings = append(bindings, p.keyMap.Scroll)
	}

	if p.hasDiffView() {
		bindings = append(
			bindings,
			p.keyMap.ToggleDiffMode,
			p.keyMap.ToggleFullscreen,
		)
	}

	return bindings
}

// FullHelp implements [help.KeyMap].
func (p *Permissions) FullHelp() [][]key.Binding {
	return [][]key.Binding{p.ShortHelp()}
}

// delegationHeaderKey labels the "who is asking" line by delegation kind,
// so the word matches what the user sees everywhere else (the dock, the
// dashboard) rather than a generic one they have to translate.
func delegationHeaderKey(kind string) string {
	switch kind {
	case string(proto.ThreadKindThread):
		return "Thread"
	case string(proto.ThreadKindTask):
		return "Task"
	default:
		return "From"
	}
}

// delegationHeaderValue prefers the delegation's name and falls back to
// its id: a thread always has a name, a task may not.
func delegationHeaderValue(ref permission.DelegationRef) string {
	if ref.Name != "" {
		return ref.Name
	}
	return ref.ID
}
