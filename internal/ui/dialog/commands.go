package dialog

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/list"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
)

// CommandsID is the identifier for the commands dialog.
const CommandsID = "commands"

// CommandType represents the type of commands being displayed.
type CommandType uint

// String returns the string representation of the CommandType.
func (c CommandType) String() string { return []string{"System", "User", "MCP"}[c] }

const sidebarCompactModeBreakpoint = 120

const (
	SystemCommands CommandType = iota
	UserCommands
	MCPPrompts
)

type dockerMCPAvailabilityCheckedMsg struct{ available bool }

// Commands composes selectDialog's input, list, navigation, help and layout
// with command-palette-specific tabs, shortcuts and asynchronous availability.
type Commands struct {
	*selectDialog
	com                            *common.Common
	sessionID                      string
	hasSession, hasTodos, hasQueue bool
	selected                       CommandType
	spinner                        spinner.Model
	loading                        bool
	windowWidth                    int
	customCommands                 []workspace.CustomCommand
	mcpPrompts                     []workspace.MCPPrompt
	dockerMCPAvailable             *bool
	dockerMCPCheckInFlight         bool
	tab, shiftTab                  key.Binding
}

var _ Dialog = (*Commands)(nil)

func NewCommands(com *common.Common, sessionID string, hasSession, hasTodos, hasQueue bool, customCommands []workspace.CustomCommand, mcpPrompts []workspace.MCPPrompt) (*Commands, error) {
	c := &Commands{com: com, sessionID: sessionID, hasSession: hasSession, hasTodos: hasTodos, hasQueue: hasQueue, customCommands: customCommands, mcpPrompts: mcpPrompts}
	if available, known := com.Workspace.DockerMCPAvailable(); known {
		c.dockerMCPAvailable = &available
	}
	sd, err := newSelectDialog(com, selectDialogConfig{id: CommandsID, title: "Commands", maxWidth: defaultDialogMaxWidth, maxHeight: defaultDialogHeight, buildItems: c.commandItems, onSelect: func(id string) Action {
		for _, item := range c.list.FilteredItems() {
			if command, ok := item.(*CommandItem); ok && command.ID() == id {
				return command.Action()
			}
		}
		return nil
	}})
	if err != nil {
		return nil, err
	}
	c.selectDialog = sd
	c.tab = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch selection"))
	c.shiftTab = key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "switch selection prev"))
	c.spinner = spinner.New()
	c.spinner.Spinner = spinner.Dot
	c.spinner.Style = com.Styles.Dialog.Spinner
	return c, nil
}

// Restyle implements [Restyler]: the shared select-dialog chrome plus the
// loading spinner, whose color is copied at construction.
func (c *Commands) Restyle() {
	c.selectDialog.Restyle()
	c.spinner.Style = c.com.Styles.Dialog.Spinner
}

func (c *Commands) commandItems() ([]list.FilterableItem, int, error) {
	var items []list.FilterableItem
	switch c.selected {
	case SystemCommands:
		for _, item := range c.defaultCommands() {
			items = append(items, item)
		}
	case UserCommands:
		for _, command := range c.customCommands {
			items = append(items, customCommandItem(c.com.Styles, command))
		}
	case MCPPrompts:
		for _, prompt := range c.mcpPrompts {
			items = append(items, mcpPromptItem(c.com.Styles, prompt))
		}
	}
	return items, 0, nil
}

func (c *Commands) ID() string { return CommandsID }

func (c *Commands) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case dockerMCPAvailabilityCheckedMsg:
		c.dockerMCPAvailable, c.dockerMCPCheckInFlight = &msg.available, false
		if c.selected == SystemCommands {
			// The item source changed out from under the user, not
			// anything they asked for — keep whatever they've typed.
			_ = c.replaceItems(c.commandItems, true)
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
		case key.Matches(msg, c.tab) && (len(c.customCommands) > 0 || len(c.mcpPrompts) > 0):
			c.selected = c.nextCommandType()
			// Switching category changes what's being listed, so an old
			// filter from the previous category could hide everything
			// here — clear it instead of carrying it forward.
			_ = c.replaceItems(c.commandItems, false)
			return nil
		case key.Matches(msg, c.shiftTab) && (len(c.customCommands) > 0 || len(c.mcpPrompts) > 0):
			c.selected = c.previousCommandType()
			_ = c.replaceItems(c.commandItems, false)
			return nil
		}
		selectAction := func(item ListItem) Action { return item.(*CommandItem).Action() }
		if action, handled := c.handleSelect(msg, selectAction); handled {
			return action
		}
		for _, item := range c.list.FilteredItems() {
			if command, ok := item.(*CommandItem); ok && msg.String() == command.Shortcut() {
				return command.Action()
			}
		}
		return c.handleNavigation(msg, selectAction)
	}
	return nil
}

// checkDockerMCPAvailabilityCmd answers whether the Docker MCP gateway is
// usable here. The answer costs a subprocess with a timeout, which is why
// it runs off the Update loop — and why it is asked of the workspace
// rather than run from the dialog.
func checkDockerMCPAvailabilityCmd(com *common.Common) tea.Cmd {
	ws := com.Workspace
	return func() tea.Msg {
		return dockerMCPAvailabilityCheckedMsg{available: ws.RefreshDockerMCPAvailability()}
	}
}

func (c *Commands) InitialCmd() tea.Cmd {
	if c.dockerMCPAvailable != nil || c.dockerMCPCheckInFlight {
		return nil
	}
	c.dockerMCPCheckInFlight = true
	return checkDockerMCPAvailabilityCmd(c.com)
}

func commandsRadioView(sty *styles.Styles, selected CommandType, hasUserCmds, hasMCPPrompts bool) string {
	if !hasUserCmds && !hasMCPPrompts {
		return ""
	}
	selectedFn := func(t CommandType) string {
		if t == selected {
			return sty.Radio.On.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
		}
		return sty.Radio.Off.Padding(0, 1).Render() + sty.Radio.Label.Render(t.String())
	}
	parts := []string{selectedFn(SystemCommands)}
	if hasUserCmds {
		parts = append(parts, selectedFn(UserCommands))
	}
	if hasMCPPrompts {
		parts = append(parts, selectedFn(MCPPrompts))
	}
	return strings.Join(parts, " ")
}

func (c *Commands) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	// Draw is the only place this dialog ever learns its width: [Dialog]
	// has no resize hook, and HandleMsg never sees a tea.WindowSizeMsg.
	// This mutation only fires when the width actually changed, so
	// repeated Draw calls for an unchanged area are idempotent — it is a
	// lazy cache, not a per-frame side effect, the same way buttonRects
	// gets captured at draw time in permissions.go and read back later.
	if area.Dx() != c.windowWidth && c.selected == SystemCommands {
		c.windowWidth = area.Dx()
		// A resize re-runs buildItems but the user didn't ask to change
		// what's listed — keep their filter.
		_ = c.replaceItems(c.commandItems, true)
	}
	t := c.com.Styles
	height := max(0, min(defaultDialogHeight, area.Dy()-t.Dialog.View.GetVerticalBorderSize()))
	layout := c.layout(area, height, false)
	width, innerWidth := layout.width, layout.innerWidth
	applyInfoColumnVisibility(c.list.FilteredItems(), layout.listWidth, commandInfoMaxPercent)
	rc := NewRenderContext(t, width)
	rc.Title = "Commands"
	rc.TitleInfo = commandsRadioView(t, c.selected, len(c.customCommands) > 0, len(c.mcpPrompts) > 0)
	rc.AddPart(t.Dialog.InputPrompt.Render(c.input.View()))
	rc.AddPart(t.Dialog.List.Height(c.list.Height()).Render(c.list.Render()))
	rc.Help = renderDialogHelp(t, &c.help, c, innerWidth)
	if c.loading {
		rc.Help = t.Dialog.HelpView.Width(innerWidth).Render(c.spinner.View() + " Generating Prompt...")
	}
	view, cur := rc.Render(), c.Cursor()
	DrawCenterCursor(scr, area, view, cur)
	return cur
}

func (c *Commands) ShortHelp() []key.Binding {
	return []key.Binding{c.tab, c.keyMap.UpDown, c.keyMap.Select, c.keyMap.Close}
}

func (c *Commands) FullHelp() [][]key.Binding {
	return [][]key.Binding{{c.keyMap.Select, c.keyMap.Next, c.keyMap.Previous, c.tab}, {c.keyMap.Close}}
}

func (c *Commands) nextCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
	case UserCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
	case MCPPrompts:
	}
	return SystemCommands
}

func (c *Commands) previousCommandType() CommandType {
	switch c.selected {
	case SystemCommands:
		if len(c.mcpPrompts) > 0 {
			return MCPPrompts
		}
		if len(c.customCommands) > 0 {
			return UserCommands
		}
	case UserCommands:
		return SystemCommands
	case MCPPrompts:
		if len(c.customCommands) > 0 {
			return UserCommands
		}
	}
	return SystemCommands
}

func (c *Commands) setCommandItems(commandType CommandType) {
	c.selected = commandType
	// setCommandItems switches (or re-selects) a category explicitly, so
	// clear the filter rather than carrying one that may not match
	// anything in the new list.
	_ = c.replaceItems(c.commandItems, false)
}

// customCommandItem builds a CommandItem for a user-defined (file-backed)
// custom command or skill.
func customCommandItem(sty *styles.Styles, cmd workspace.CustomCommand) *CommandItem {
	var action Action
	if cmd.Skill != nil {
		action = ActionAttachSkill{ID: cmd.Skill.SkillFilePath, Name: cmd.Skill.Name}
	} else {
		action = ActionRunCustomCommand{
			Content:   cmd.Content,
			Arguments: cmd.Arguments,
		}
	}
	item := NewCommandItem(sty, "custom_"+cmd.ID, cmd.Name, "", action)
	if cmd.Skill != nil {
		item = item.WithDescription(cmd.Skill.Description)
	}
	return item
}

// mcpPromptItem builds a CommandItem for a prompt exposed by an MCP server.
func mcpPromptItem(sty *styles.Styles, cmd workspace.MCPPrompt) *CommandItem {
	action := ActionRunMCPPrompt{
		Title:       cmd.Title,
		Description: cmd.Description,
		PromptID:    cmd.PromptID,
		ClientID:    cmd.ClientID,
		Arguments:   cmd.Arguments,
	}
	item := NewCommandItem(sty, "mcp_"+cmd.ID, cmd.PromptID, "", action)
	if cmd.Description != "" {
		item = item.WithDescription(cmd.Description)
	}
	return item
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
	customCommands []workspace.CustomCommand,
	mcpPrompts []workspace.MCPPrompt,
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
		NewCommandItem(sty, "new_session", "new", "ctrl+n", ActionNewSession{}).WithAliases("new session", "clear").WithDescription("start a new session"),
		NewCommandItem(sty, "switch_session", "sessions", "ctrl+s", ActionOpenDialog{SessionsID}).WithDescription("switch session"),
		NewCommandItem(sty, "switch_model", "models", "ctrl+l", ActionOpenDialog{ModelsID}).WithAliases("switch model", "model").WithDescription("switch model"),
		NewCommandItem(sty, "configure_providers", "providers", "", ActionOpenDialog{ProvidersID}).WithAliases("configure providers").WithDescription("configure providers"),
		NewCommandItem(sty, "doctor", "doctor", "", ActionOpenDialog{DoctorID}).WithDescription("check config problems"),
		NewCommandItem(sty, "stats", "stats", "", ActionOpenDialog{StatsID}).WithAliases("usage", "tokens", "cost").WithDescription("usage by model and subagent"),
		NewCommandItem(sty, "select_theme", "theme", "", ActionOpenDialog{ThemeID}).WithAliases("switch theme", "colors", "palette").WithDescription("switch color theme"),
	}

	// Only offer the threads dashboard for workspaces that actually own a
	// thread manager (see workspace.Workspace.SupportsThreads).
	if com.Workspace != nil && com.Workspace.SupportsThreads() {
		commands = append(commands, NewCommandItem(sty, "threads", "threads", "ctrl+e", ActionOpenThreadsDashboard{}).
			WithAliases("monitor work threads", "thread dashboard").
			WithDescription("monitor work threads"))
	}

	// Only show compact command if there's an active session
	if hasSession {
		commands = append(commands, NewCommandItem(sty, "summarize", "compact", "", ActionSummarize{SessionID: sessionID}).
			WithAliases("summarize", "summarize session").
			WithDescription("summarize the session"))
	}

	// Add reasoning toggle for models that support it
	cfg := com.Config()
	// The coder agent leaves Model unset (it inherits the app's configured
	// model), so the model it actually runs on is always cfg.Model.
	if _, ok := cfg.Agents[config.AgentCoder]; ok {
		providerCfg := cfg.GetProviderForModel()
		model := cfg.SelectedCatalogModel()
		if providerCfg != nil && model != nil && model.CanReason {
			selectedModel := cfg.Model

			// Anthropic models: thinking toggle
			if model.CanReason && len(model.ReasoningLevels) == 0 {
				status := "enable"
				if selectedModel.Think {
					status = "disable"
				}
				commands = append(commands, NewCommandItem(sty, "toggle_thinking", "thinking", "", ActionToggleThinking{}).
					WithAliases(status+" thinking mode", "toggle thinking").
					WithDescription("toggle thinking mode"))
			}

			// OpenAI models: reasoning effort dialog
			if len(model.ReasoningLevels) > 0 {
				commands = append(commands, NewCommandItem(sty, "select_reasoning_effort", "effort", "", ActionOpenDialog{
					DialogID: ReasoningID,
				}).WithAliases("select reasoning effort", "reasoning effort").WithDescription("reasoning effort"))
			}
		}
	}
	// Only show toggle compact mode command if window width is larger than compact breakpoint (120)
	if windowWidth >= sidebarCompactModeBreakpoint && hasSession {
		commands = append(commands, NewCommandItem(sty, "toggle_sidebar", "sidebar", "", ActionToggleCompactMode{}).WithAliases("toggle sidebar").WithDescription("toggle sidebar"))
	}
	if hasSession {
		// See the reasoning-toggle block above: the coder inherits the
		// configured model.
		model := cfg.SelectedCatalogModel()
		if model != nil && model.SupportsImages {
			commands = append(commands, NewCommandItem(sty, "file_picker", "files", "ctrl+f", ActionOpenDialog{
				DialogID: FilePickerID,
			}).WithAliases("open file picker", "file picker").WithDescription("attach a file"))
		}
	}

	// Add external editor command if $EDITOR is available.
	//
	// TODO: Use [tea.EnvMsg] to get environment variable instead of os.Getenv;
	// because os.Getenv does IO is breaks the TEA paradigm and is generally an
	// antipattern.
	if os.Getenv("EDITOR") != "" {
		commands = append(commands, NewCommandItem(sty, "open_external_editor", "editor", "ctrl+o", ActionExternalEditor{}).
			WithAliases("open external editor", "external editor").
			WithDescription("open external editor"))
	}

	// Add Docker MCP command if available and not already enabled.
	if !cfg.IsDockerMCPEnabled() && dockerMCPAvailable != nil && *dockerMCPAvailable {
		commands = append(commands, NewCommandItem(sty, "enable_docker_mcp", "enable docker mcp", "", ActionEnableDockerMCP{}).
			WithAliases("enable docker mcp catalog").
			WithDescription("enable docker mcp catalog"))
	}

	// Add disable Docker MCP command if it's currently enabled
	if cfg.IsDockerMCPEnabled() {
		commands = append(commands, NewCommandItem(sty, "disable_docker_mcp", "disable docker mcp", "", ActionDisableDockerMCP{}).
			WithAliases("disable docker mcp catalog").
			WithDescription("disable docker mcp catalog"))
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
		commands = append(commands, NewCommandItem(sty, "toggle_pills", "todos", "ctrl+t", ActionTogglePills{}).WithAliases(label, "todos/queue").WithDescription("toggle to-dos"))
	}

	// Add a command for selecting notification style via picker dialog.
	commands = append(commands, NewCommandItem(sty, "select_notifications", "notifications", "", ActionOpenDialog{DialogID: NotificationsID}).
		WithAliases("notification style").
		WithDescription("notification style"))

	commands = append(
		commands,
		NewCommandItem(sty, "toggle_yolo", "yolo", "ctrl+y", ActionToggleYoloMode{}).WithAliases("toggle yolo mode").WithDescription("skip permission prompts"),
		NewCommandItem(sty, "toggle_help", "help", "ctrl+g", ActionToggleHelp{}).WithAliases("toggle help").WithDescription("toggle help"),
		NewCommandItem(sty, "init", "init", "", ActionInitializeProject{}).WithAliases("initialize project").WithDescription("initialize project"),
	)

	// Add transparent background toggle.
	transparentAlias := "disable background color"
	if cfg.TransparentEnabled() {
		transparentAlias = "enable background color"
	}
	commands = append(commands, NewCommandItem(sty, "toggle_transparent", "transparency", "", ActionToggleTransparentBackground{}).
		WithAliases(transparentAlias, "background color").
		WithDescription("toggle background"))

	commands = append(
		commands,
		NewCommandItem(sty, "quit", "exit", "ctrl+c", tea.QuitMsg{}).WithAliases("quit").WithDescription("quit sennit"),
	)

	return commands
}

// SetCustomCommands sets the custom commands and refreshes the view if user commands are currently displayed.
func (c *Commands) SetCustomCommands(customCommands []workspace.CustomCommand) {
	c.customCommands = customCommands
	if c.selected == UserCommands {
		c.setCommandItems(c.selected)
	}
}

// SetMCPPrompts sets the MCP prompts and refreshes the view if MCP prompts are currently displayed.
func (c *Commands) SetMCPPrompts(mcpPrompts []workspace.MCPPrompt) {
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
