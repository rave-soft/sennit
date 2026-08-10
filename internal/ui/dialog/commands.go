package dialog

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/list"
	"github.com/rave-soft/braid/internal/ui/styles"
)

// CommandsID is the identifier for the commands dialog.
const CommandsID = "commands"

// CommandType represents the type of commands being displayed.
type CommandType uint

// String returns the string representation of the CommandType.
func (c CommandType) String() string { return []string{"System", "User", "MCP"}[c] }

const (
	sidebarCompactModeBreakpoint = 120
)

const (
	SystemCommands CommandType = iota
	UserCommands
	MCPPrompts
)

// Commands represents a dialog that shows available commands.
type dockerMCPAvailabilityCheckedMsg struct {
	available bool
}

type Commands struct {
	com    *common.Common
	keyMap struct {
		Select,
		UpDown,
		Next,
		Previous,
		Tab,
		ShiftTab,
		Close key.Binding
	}

	sessionID  string
	hasSession bool
	hasTodos   bool
	hasQueue   bool
	selected   CommandType

	spinner spinner.Model
	loading bool

	help  help.Model
	input textinput.Model
	list  *list.FilterableList

	windowWidth int

	customCommands []commands.CustomCommand
	mcpPrompts     []commands.MCPPrompt

	dockerMCPAvailable     *bool
	dockerMCPCheckInFlight bool
}

var _ Dialog = (*Commands)(nil)

// NewCommands creates a new commands dialog.
func NewCommands(com *common.Common, sessionID string, hasSession, hasTodos, hasQueue bool, customCommands []commands.CustomCommand, mcpPrompts []commands.MCPPrompt) (*Commands, error) {
	c := &Commands{
		com:            com,
		selected:       SystemCommands,
		sessionID:      sessionID,
		hasSession:     hasSession,
		hasTodos:       hasTodos,
		hasQueue:       hasQueue,
		customCommands: customCommands,
		mcpPrompts:     mcpPrompts,
	}

	help := help.New()
	help.Styles = com.Styles.DialogHelpStyles()

	c.help = help

	c.list = list.NewFilterableList()
	c.list.Focus()
	c.list.SetSelected(0)

	c.input = textinput.New()
	c.input.SetVirtualCursor(false)
	c.input.Placeholder = "Type to filter"
	c.input.SetStyles(com.Styles.TextInput)
	c.input.Focus()

	c.keyMap.Select = key.NewBinding(
		key.WithKeys("enter", "ctrl+y"),
		key.WithHelp("enter", "confirm"),
	)
	c.keyMap.UpDown = key.NewBinding(
		key.WithKeys("up", "down"),
		key.WithHelp("↑/↓", "choose"),
	)
	c.keyMap.Next = key.NewBinding(
		key.WithKeys("down"),
		key.WithHelp("↓", "next item"),
	)
	c.keyMap.Previous = key.NewBinding(
		key.WithKeys("up", "ctrl+p"),
		key.WithHelp("↑", "previous item"),
	)
	c.keyMap.Tab = key.NewBinding(
		key.WithKeys("tab"),
		key.WithHelp("tab", "switch selection"),
	)
	c.keyMap.ShiftTab = key.NewBinding(
		key.WithKeys("shift+tab"),
		key.WithHelp("shift+tab", "switch selection prev"),
	)
	closeKey := CloseKey
	closeKey.SetHelp("esc", "cancel")
	c.keyMap.Close = closeKey

	if available, known := config.DockerMCPAvailabilityCached(); known {
		c.dockerMCPAvailable = &available
	}

	// Set initial commands
	c.setCommandItems(c.selected)

	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = com.Styles.Dialog.Spinner
	c.spinner = s

	return c, nil
}

// ID implements Dialog.
func (c *Commands) ID() string {
	return CommandsID
}

// HandleMsg implements [Dialog].
func (c *Commands) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case dockerMCPAvailabilityCheckedMsg:
		c.dockerMCPAvailable = &msg.available
		c.dockerMCPCheckInFlight = false
		if c.selected == SystemCommands {
			// Preserve the current selection across the rebuild to avoid reset
			var prevID string
			if item, ok := c.list.SelectedItem().(*CommandItem); ok && item != nil {
				prevID = item.id
			}
			c.setCommandItems(c.selected)
			if prevID != "" {
				for i, it := range c.list.FilteredItems() {
					if ci, ok := it.(*CommandItem); ok && ci != nil && ci.id == prevID {
						c.list.SetSelected(i)
						c.list.ScrollToSelected()
						break
					}
				}
			}
		}
		return nil
	case spinner.TickMsg:
		if c.loading {
			var cmd tea.Cmd
			c.spinner, cmd = c.spinner.Update(msg)
			return ActionCmd{Cmd: cmd}
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, c.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, c.keyMap.Previous):
			c.list.Focus()
			if c.list.IsSelectedFirst() {
				c.list.SelectLast()
			} else {
				c.list.SelectPrev()
			}
			c.list.ScrollToSelected()
		case key.Matches(msg, c.keyMap.Next):
			c.list.Focus()
			if c.list.IsSelectedLast() {
				c.list.SelectFirst()
			} else {
				c.list.SelectNext()
			}
			c.list.ScrollToSelected()
		case key.Matches(msg, c.keyMap.Select):
			if selectedItem := c.list.SelectedItem(); selectedItem != nil {
				if item, ok := selectedItem.(*CommandItem); ok && item != nil {
					return item.Action()
				}
			}
		case key.Matches(msg, c.keyMap.Tab):
			if len(c.customCommands) > 0 || len(c.mcpPrompts) > 0 {
				c.selected = c.nextCommandType()
				c.setCommandItems(c.selected)
			}
		case key.Matches(msg, c.keyMap.ShiftTab):
			if len(c.customCommands) > 0 || len(c.mcpPrompts) > 0 {
				c.selected = c.previousCommandType()
				c.setCommandItems(c.selected)
			}
		default:
			var cmd tea.Cmd
			for _, item := range c.list.FilteredItems() {
				if item, ok := item.(*CommandItem); ok && item != nil {
					if msg.String() == item.Shortcut() {
						return item.Action()
					}
				}
			}
			c.input, cmd = c.input.Update(msg)
			value := c.input.Value()
			c.list.SetFilter(value)
			c.list.ScrollToTop()
			c.list.SetSelected(0)
			return ActionCmd{cmd}
		}
	}
	return nil
}

func checkDockerMCPAvailabilityCmd() tea.Cmd {
	return func() tea.Msg {
		return dockerMCPAvailabilityCheckedMsg{available: config.RefreshDockerMCPAvailability()}
	}
}

func (c *Commands) InitialCmd() tea.Cmd {
	if c.dockerMCPAvailable != nil || c.dockerMCPCheckInFlight {
		return nil
	}
	c.dockerMCPCheckInFlight = true
	return checkDockerMCPAvailabilityCmd()
}

// Cursor returns the cursor position relative to the dialog.
func (c *Commands) Cursor() *tea.Cursor {
	return InputCursor(c.com.Styles, c.input.Cursor())
}

// commandsRadioView generates the command type selector radio buttons.
func commandsRadioView(sty *styles.Styles, selected CommandType, hasUserCmds bool, hasMCPPrompts bool) string {
	if !hasUserCmds && !hasMCPPrompts {
		return ""
	}

	selectedFn := func(t CommandType) string {
		if t == selected {
			return sty.Radio.On.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
		}
		return sty.Radio.Off.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
	}

	parts := []string{
		selectedFn(SystemCommands),
	}

	if hasUserCmds {
		parts = append(parts, selectedFn(UserCommands))
	}
	if hasMCPPrompts {
		parts = append(parts, selectedFn(MCPPrompts))
	}

	return strings.Join(parts, " ")
}

// Draw implements [Dialog].
func (c *Commands) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := c.com.Styles
	width := max(0, min(defaultDialogMaxWidth, area.Dx()-t.Dialog.View.GetHorizontalBorderSize()))
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	if area.Dx() != c.windowWidth && c.selected == SystemCommands {
		c.windowWidth = area.Dx()
		// since some items in the list depend on width (e.g. toggle sidebar command),
		// we need to reset the command items when width changes
		c.setCommandItems(c.selected)
	}

	innerWidth := width - c.com.Styles.Dialog.View.GetHorizontalFrameSize()
	heightOffset := t.Dialog.Title.GetVerticalFrameSize() + titleContentHeight +
		t.Dialog.InputPrompt.GetVerticalFrameSize() + inputContentHeight +
		t.Dialog.HelpView.GetVerticalFrameSize() +
		t.Dialog.View.GetVerticalFrameSize()

	c.input.SetWidth(dialogInputTextWidth(t, c.input, innerWidth))

	c.list.SetSize(innerWidth, max(0, height-heightOffset))

	// Hide the shortcut hints uniformly when the widest would crowd names.
	applyInfoColumnVisibility(c.list.FilteredItems(), innerWidth, commandInfoMaxPercent)

	rc := NewRenderContext(t, width)
	rc.Title = "Commands"
	rc.TitleInfo = commandsRadioView(t, c.selected, len(c.customCommands) > 0, len(c.mcpPrompts) > 0)
	inputView := t.Dialog.InputPrompt.Render(c.input.View())
	rc.AddPart(inputView)
	listView := t.Dialog.List.Height(c.list.Height()).Render(c.list.Render())
	rc.AddPart(listView)
	rc.Help = renderDialogHelp(t, &c.help, c, innerWidth)

	if c.loading {
		rc.Help = t.Dialog.HelpView.Width(innerWidth).Render(c.spinner.View() + " Generating Prompt...")
	}

	view := rc.Render()

	cur := c.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

// ShortHelp implements [help.KeyMap].
func (c *Commands) ShortHelp() []key.Binding {
	return []key.Binding{
		c.keyMap.Tab,
		c.keyMap.UpDown,
		c.keyMap.Select,
		c.keyMap.Close,
	}
}

// FullHelp implements [help.KeyMap].
func (c *Commands) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{c.keyMap.Select, c.keyMap.Next, c.keyMap.Previous, c.keyMap.Tab},
		{c.keyMap.Close},
	}
}

// nextCommandType returns the next command type in the cycle.
func (c *Commands) nextCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		fallthrough
	case UserCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		fallthrough
	case MCPPrompts:
		return SystemCommands
	default:
		return SystemCommands
	}
}

// previousCommandType returns the previous command type in the cycle.
func (c *Commands) previousCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		return SystemCommands
	case UserCommands:
		return SystemCommands
	case MCPPrompts:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		return SystemCommands
	default:
		return SystemCommands
	}
}

// setCommandItems sets the command items based on the specified command type.
func (c *Commands) setCommandItems(commandType CommandType) {
	c.selected = commandType

	commandItems := []list.FilterableItem{}
	switch c.selected {
	case SystemCommands:
		for _, cmd := range c.defaultCommands() {
			commandItems = append(commandItems, cmd)
		}
	case UserCommands:
		for _, cmd := range c.customCommands {
			commandItems = append(commandItems, customCommandItem(c.com.Styles, cmd))
		}
	case MCPPrompts:
		for _, cmd := range c.mcpPrompts {
			commandItems = append(commandItems, mcpPromptItem(c.com.Styles, cmd))
		}
	}

	c.list.SetItems(commandItems...)
	c.list.SetFilter("")
	c.list.ScrollToTop()
	c.list.SetSelected(0)
	c.input.SetValue("")
}

// customCommandItem builds a CommandItem for a user-defined (file-backed)
// custom command or skill.
func customCommandItem(sty *styles.Styles, cmd commands.CustomCommand) *CommandItem {
	var action Action
	if cmd.Skill != nil {
		action = ActionAttachSkill{ID: cmd.Skill.SkillFilePath, Name: cmd.Skill.Name}
	} else {
		action = ActionRunCustomCommand{
			Content:   cmd.Content,
			Arguments: cmd.Arguments,
			Skill:     cmd.Skill,
		}
	}
	item := NewCommandItem(sty, "custom_"+cmd.ID, cmd.Name, "", action)
	if cmd.Skill != nil {
		item = item.WithDescription(cmd.Skill.Description)
	}
	return item
}

// mcpPromptItem builds a CommandItem for a prompt exposed by an MCP server.
func mcpPromptItem(sty *styles.Styles, cmd commands.MCPPrompt) *CommandItem {
	action := ActionRunMCPPrompt{
		Title:       cmd.Title,
		Description: cmd.Description,
		PromptID:    cmd.PromptID,
		ClientID:    cmd.ClientID,
		Arguments:   cmd.Arguments,
	}
	return NewCommandItem(sty, "mcp_"+cmd.ID, cmd.PromptID, "", action)
}

// BuildCommandItems returns the flat list of every command available right
// now — built-in system commands, user-defined custom commands, and MCP
// prompts — combined into one slice. It is the single source of truth
// shared by the Commands palette dialog and the editor's "/" completion
// popup, so the two never drift out of sync.
func BuildCommandItems(
	com *common.Common,
	sessionID string,
	hasSession, hasTodos, hasQueue bool,
	windowWidth int,
	dockerMCPAvailable *bool,
	customCommands []commands.CustomCommand,
	mcpPrompts []commands.MCPPrompt,
) []*CommandItem {
	items := systemCommandItems(com, sessionID, hasSession, hasTodos, hasQueue, windowWidth, dockerMCPAvailable)
	for _, cmd := range customCommands {
		items = append(items, customCommandItem(com.Styles, cmd))
	}
	for _, cmd := range mcpPrompts {
		items = append(items, mcpPromptItem(com.Styles, cmd))
	}
	return items
}

// defaultCommands returns the list of default system commands.
func (c *Commands) defaultCommands() []*CommandItem {
	return systemCommandItems(c.com, c.sessionID, c.hasSession, c.hasTodos, c.hasQueue, c.windowWidth, c.dockerMCPAvailable)
}

// systemCommandItems builds the built-in (non-custom, non-MCP) commands.
// Titles are short, typed-after-"/" names (Claude Code / opencode style);
// long-form aliases are kept via WithAliases so they still match filtering.
func systemCommandItems(com *common.Common, sessionID string, hasSession, hasTodos, hasQueue bool, windowWidth int, dockerMCPAvailable *bool) []*CommandItem {
	sty := com.Styles
	commands := []*CommandItem{
		NewCommandItem(sty, "new_session", "new", "ctrl+n", ActionNewSession{}).WithAliases("new session", "clear"),
		NewCommandItem(sty, "switch_session", "sessions", "ctrl+s", ActionOpenDialog{SessionsID}),
		NewCommandItem(sty, "switch_model", "models", "ctrl+l", ActionOpenDialog{ModelsID}).WithAliases("switch model", "model"),
		NewCommandItem(sty, "configure_providers", "providers", "", ActionOpenDialog{ProvidersID}).WithAliases("configure providers"),
	}

	// Only show compact command if there's an active session
	if hasSession {
		commands = append(commands, NewCommandItem(sty, "summarize", "compact", "", ActionSummarize{SessionID: sessionID}).WithAliases("summarize", "summarize session"))
	}

	// Add reasoning toggle for models that support it
	cfg := com.Config()
	// The coder agent leaves Model unset (it inherits the app's main model),
	// so the model it actually runs on always lives in the large slot.
	if _, ok := cfg.Agents[config.AgentCoder]; ok {
		providerCfg := cfg.GetProviderForModel(config.SelectedModelTypeLarge)
		model := cfg.GetModelByType(config.SelectedModelTypeLarge)
		if providerCfg != nil && model != nil && model.CanReason {
			selectedModel := cfg.Models[config.SelectedModelTypeLarge]

			// Anthropic models: thinking toggle
			if model.CanReason && len(model.ReasoningLevels) == 0 {
				status := "enable"
				if selectedModel.Think {
					status = "disable"
				}
				commands = append(commands, NewCommandItem(sty, "toggle_thinking", "thinking", "", ActionToggleThinking{}).
					WithAliases(status+" thinking mode", "toggle thinking").
					WithDescription(status+" thinking mode"))
			}

			// OpenAI models: reasoning effort dialog
			if len(model.ReasoningLevels) > 0 {
				commands = append(commands, NewCommandItem(sty, "select_reasoning_effort", "effort", "", ActionOpenDialog{
					DialogID: ReasoningID,
				}).WithAliases("select reasoning effort", "reasoning effort"))
			}
		}
	}
	// Only show toggle compact mode command if window width is larger than compact breakpoint (120)
	if windowWidth >= sidebarCompactModeBreakpoint && hasSession {
		commands = append(commands, NewCommandItem(sty, "toggle_sidebar", "sidebar", "", ActionToggleCompactMode{}).WithAliases("toggle sidebar"))
	}
	if hasSession {
		// See the reasoning-toggle block above: the coder inherits the main
		// (large) model.
		model := cfg.GetModelByType(config.SelectedModelTypeLarge)
		if model != nil && model.SupportsImages {
			commands = append(commands, NewCommandItem(sty, "file_picker", "files", "ctrl+f", ActionOpenDialog{
				DialogID: FilePickerID,
			}).WithAliases("open file picker", "file picker"))
		}
	}

	// Add external editor command if $EDITOR is available.
	//
	// TODO: Use [tea.EnvMsg] to get environment variable instead of os.Getenv;
	// because os.Getenv does IO is breaks the TEA paradigm and is generally an
	// antipattern.
	if os.Getenv("EDITOR") != "" {
		commands = append(commands, NewCommandItem(sty, "open_external_editor", "editor", "ctrl+o", ActionExternalEditor{}).WithAliases("open external editor", "external editor"))
	}

	// Add Docker MCP command if available and not already enabled.
	if !cfg.IsDockerMCPEnabled() && dockerMCPAvailable != nil && *dockerMCPAvailable {
		commands = append(commands, NewCommandItem(sty, "enable_docker_mcp", "enable docker mcp", "", ActionEnableDockerMCP{}).WithAliases("enable docker mcp catalog"))
	}

	// Add disable Docker MCP command if it's currently enabled
	if cfg.IsDockerMCPEnabled() {
		commands = append(commands, NewCommandItem(sty, "disable_docker_mcp", "disable docker mcp", "", ActionDisableDockerMCP{}).WithAliases("disable docker mcp catalog"))
	}

	if hasTodos || hasQueue {
		var label string
		switch {
		case hasTodos && hasQueue:
			label = "toggle to-dos/queue"
		case hasQueue:
			label = "toggle queue"
		default:
			label = "toggle to-dos"
		}
		commands = append(commands, NewCommandItem(sty, "toggle_pills", "todos", "ctrl+t", ActionTogglePills{}).WithAliases(label, "todos/queue"))
	}

	// Add a command for selecting notification style via picker dialog.
	commands = append(commands, NewCommandItem(sty, "select_notifications", "notifications", "", ActionOpenDialog{DialogID: NotificationsID}).WithAliases("notification style"))

	commands = append(
		commands,
		NewCommandItem(sty, "toggle_yolo", "yolo", "ctrl+y", ActionToggleYoloMode{}).WithAliases("toggle yolo mode"),
		NewCommandItem(sty, "toggle_help", "help", "ctrl+g", ActionToggleHelp{}).WithAliases("toggle help"),
		NewCommandItem(sty, "init", "init", "", ActionInitializeProject{}).WithAliases("initialize project"),
	)

	// Add transparent background toggle.
	transparentAlias := "disable background color"
	if cfg != nil && cfg.Options != nil && cfg.Options.TUI.Transparent != nil && *cfg.Options.TUI.Transparent {
		transparentAlias = "enable background color"
	}
	commands = append(commands, NewCommandItem(sty, "toggle_transparent", "transparency", "", ActionToggleTransparentBackground{}).
		WithAliases(transparentAlias, "background color").
		WithDescription(transparentAlias))

	commands = append(
		commands,
		NewCommandItem(sty, "quit", "exit", "ctrl+c", tea.QuitMsg{}).WithAliases("quit"),
	)

	return commands
}

// SetCustomCommands sets the custom commands and refreshes the view if user commands are currently displayed.
func (c *Commands) SetCustomCommands(customCommands []commands.CustomCommand) {
	c.customCommands = customCommands
	if c.selected == UserCommands {
		c.setCommandItems(c.selected)
	}
}

// SetMCPPrompts sets the MCP prompts and refreshes the view if MCP prompts are currently displayed.
func (c *Commands) SetMCPPrompts(mcpPrompts []commands.MCPPrompt) {
	c.mcpPrompts = mcpPrompts
	if c.selected == MCPPrompts {
		c.setCommandItems(c.selected)
	}
}

// StartLoading implements [LoadingDialog].
func (c *Commands) StartLoading() tea.Cmd {
	if c.loading {
		return nil
	}
	c.loading = true
	return c.spinner.Tick
}

// StopLoading implements [LoadingDialog].
func (c *Commands) StopLoading() {
	c.loading = false
}
