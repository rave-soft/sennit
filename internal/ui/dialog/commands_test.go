package dialog

import (
	"image"
	"testing"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/commands"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// newTestCommands builds a Commands dialog with the given custom commands
// and MCP prompts, ready to drive nextCommandType/previousCommandType and
// the tab-related plumbing.
func newTestCommands(t *testing.T, custom []commands.CustomCommand, mcpPrompts []commands.MCPPrompt) *Commands {
	t.Helper()
	com := newCommandsNamesTestCommon(t)
	c, err := NewCommands(com, "sess-1", true, false, false, custom, mcpPrompts)
	require.NoError(t, err)
	return c
}

// ---- CommandType.String ----

// TestCommandTypeString pins the label for each command category.
func TestCommandTypeString(t *testing.T) {
	t.Parallel()

	require.Equal(t, "System", SystemCommands.String())
	require.Equal(t, "User", UserCommands.String())
	require.Equal(t, "MCP", MCPPrompts.String())
}

// ---- Commands.ID ----

func TestCommands_ID(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	require.Equal(t, CommandsID, c.ID())
}

// ---- Commands.Restyle ----

// TestCommands_Restyle verifies Restyle copies the (possibly new) theme's
// spinner style, on top of the shared selectDialog restyle.
func TestCommands_Restyle(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)

	newStyles := styles.Theme("graphite-amber")
	c.com.Styles = &newStyles
	c.Restyle()

	require.Equal(t, newStyles.Dialog.Spinner, c.spinner.Style)
}

// ---- checkDockerMCPAvailabilityCmd ----

// TestCheckDockerMCPAvailabilityCmd verifies the returned tea.Cmd yields a
// dockerMCPAvailabilityCheckedMsg (the availability bool itself depends on
// whether docker is installed in the test environment, so only the message
// shape is pinned).
func TestCheckDockerMCPAvailabilityCmd(t *testing.T) {
	t.Parallel()

	cmd := checkDockerMCPAvailabilityCmd()
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(dockerMCPAvailabilityCheckedMsg)
	require.True(t, ok, "expected dockerMCPAvailabilityCheckedMsg, got %T", msg)
}

// ---- Commands.InitialCmd ----

// TestCommands_InitialCmd verifies the docker-availability probe fires
// exactly once: not at all once availability is known, and not twice if
// already in flight.
func TestCommands_InitialCmd(t *testing.T) {
	t.Parallel()

	t.Run("known availability skips the probe", func(t *testing.T) {
		t.Parallel()
		c := newTestCommands(t, nil, nil)
		available := true
		c.dockerMCPAvailable = &available

		require.Nil(t, c.InitialCmd())
		require.False(t, c.dockerMCPCheckInFlight)
	})

	t.Run("in-flight check skips a second probe", func(t *testing.T) {
		t.Parallel()
		c := newTestCommands(t, nil, nil)
		c.dockerMCPCheckInFlight = true

		require.Nil(t, c.InitialCmd())
	})

	t.Run("unknown availability starts the probe", func(t *testing.T) {
		t.Parallel()
		c := newTestCommands(t, nil, nil)
		// NewCommands seeds dockerMCPAvailable from the process-wide docker
		// availability cache (see config.DockerMCPAvailabilityCached),
		// which other tests/processes may have already warmed. Force the
		// "nothing known yet" state directly rather than depend on that
		// global's contents.
		c.dockerMCPAvailable = nil
		c.dockerMCPCheckInFlight = false

		cmd := c.InitialCmd()
		require.NotNil(t, cmd)
		require.True(t, c.dockerMCPCheckInFlight, "must mark the check in-flight before returning")
	})
}

// ---- commandsRadioView ----

// TestCommandsRadioView verifies the radio view is empty when there is
// nothing to switch between, and otherwise lists exactly the categories
// that have content, labeling the selected one.
func TestCommandsRadioView(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()

	require.Empty(t, commandsRadioView(&s, SystemCommands, false, false))

	onlyUser := ansi.Strip(commandsRadioView(&s, SystemCommands, true, false))
	require.Contains(t, onlyUser, "System")
	require.Contains(t, onlyUser, "User")
	require.NotContains(t, onlyUser, "MCP")

	all := ansi.Strip(commandsRadioView(&s, MCPPrompts, true, true))
	require.Contains(t, all, "System")
	require.Contains(t, all, "User")
	require.Contains(t, all, "MCP")
}

// ---- Commands.Draw ----

// TestCommands_Draw verifies Draw renders the title and current category
// without panicking, and returns a cursor.
func TestCommands_Draw(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, []commands.CustomCommand{{ID: "c1", Name: "custom"}}, nil)
	area := image.Rect(0, 0, 60, 20)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())

	var cur *tea.Cursor
	require.NotPanics(t, func() { cur = c.Draw(scr, area) })
	rendered := scr.String()
	require.Contains(t, rendered, "Commands")
	_ = cur

	// Loading state swaps the help footer for the spinner line.
	c.loading = true
	require.NotPanics(t, func() { c.Draw(scr, area) })
	require.Contains(t, ansi.Strip(scr.String()), "Generating Prompt...")
}

// TestCommands_DrawResizePreservesFilterText is the regression test for
// replaceItems unconditionally clearing the filter: Draw calls replaceItems
// whenever the dialog's width changes (see its area.Dx() != c.windowWidth
// check), which used to reset the filter text to "" — so simply resizing
// the terminal while typing a filter silently erased it.
func TestCommands_DrawResizePreservesFilterText(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	area := image.Rect(0, 0, 60, 20)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	c.Draw(scr, area) // establishes c.windowWidth for the check below

	c.input.SetValue("git")
	c.list.SetFilter("git")

	area2 := image.Rect(0, 0, 70, 20) // different width triggers replaceItems
	scr2 := uv.NewScreenBuffer(area2.Dx(), area2.Dy())
	require.NotPanics(t, func() { c.Draw(scr2, area2) })

	require.Equal(t, "git", c.input.Value(), "a resize must not clear the typed filter")
}

// TestCommands_DockerAvailabilityMsgPreservesFilterText is the regression
// test for the same replaceItems bug reached through
// dockerMCPAvailabilityCheckedMsg: the async Docker MCP check landing
// while the user is filtering used to wipe out whatever they had typed.
func TestCommands_DockerAvailabilityMsgPreservesFilterText(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	c.input.SetValue("bash")
	c.list.SetFilter("bash")

	c.HandleMsg(dockerMCPAvailabilityCheckedMsg{available: true})

	require.Equal(t, "bash", c.input.Value(), "an async availability update must not clear the typed filter")
}

// TestCommands_TabSwitchClearsFilterText covers the other side of
// replaceItems' preserveFilter choice: a category switch (tab/shift+tab)
// changes what's being listed, so carrying an old filter forward could
// silently show an empty list in the new category. Unlike a resize or an
// async data refresh, this is a deliberate switch, so the filter should be
// cleared.
func TestCommands_TabSwitchClearsFilterText(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, []commands.CustomCommand{{ID: "c1", Name: "custom"}}, nil)
	c.input.SetValue("bash")
	c.list.SetFilter("bash")

	c.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})

	require.Equal(t, UserCommands, c.selected, "sanity: the tab press must have switched category")
	require.Equal(t, "", c.input.Value(), "switching category must clear the previous category's filter")
}

// ---- Commands.ShortHelp / FullHelp ----

func TestCommands_ShortHelp(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	help := c.ShortHelp()
	require.Len(t, help, 4)
	require.Equal(t, c.tab, help[0])
}

func TestCommands_FullHelp(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	full := c.FullHelp()
	require.Len(t, full, 2)
	require.Equal(t, []key.Binding{c.keyMap.Select, c.keyMap.Next, c.keyMap.Previous, c.tab}, full[0])
	require.Equal(t, []key.Binding{c.keyMap.Close}, full[1])
}

// ---- nextCommandType / previousCommandType ----

// TestCommandType_CycleBothCategoriesPresent covers the common case where
// user commands and MCP prompts both exist: forward and backward from
// every category, including wrap-around at both ends.
func TestCommandType_CycleBothCategoriesPresent(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t,
		[]commands.CustomCommand{{ID: "c1", Name: "custom"}},
		[]commands.MCPPrompt{{ID: "m1", PromptID: "prompt"}},
	)

	c.selected = SystemCommands
	require.Equal(t, UserCommands, c.nextCommandType())
	c.selected = UserCommands
	require.Equal(t, MCPPrompts, c.nextCommandType())
	c.selected = MCPPrompts
	require.Equal(t, SystemCommands, c.nextCommandType(), "forward wraps from the last category to the first")

	c.selected = SystemCommands
	require.Equal(t, MCPPrompts, c.previousCommandType(), "backward wraps from the first category to the last")
	c.selected = MCPPrompts
	require.Equal(t, UserCommands, c.previousCommandType())
	c.selected = UserCommands
	require.Equal(t, SystemCommands, c.previousCommandType())
}

// TestCommandType_CycleOnlyUserCommands covers the case with custom
// commands but no MCP prompts: MCPPrompts must be skipped entirely by both
// directions.
func TestCommandType_CycleOnlyUserCommands(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, []commands.CustomCommand{{ID: "c1", Name: "custom"}}, nil)

	c.selected = SystemCommands
	require.Equal(t, UserCommands, c.nextCommandType())
	c.selected = UserCommands
	require.Equal(t, SystemCommands, c.nextCommandType(), "no MCP prompts: next from User wraps straight to System")

	c.selected = SystemCommands
	require.Equal(t, UserCommands, c.previousCommandType(), "no MCP prompts: previous from System wraps straight to User")
	c.selected = UserCommands
	require.Equal(t, SystemCommands, c.previousCommandType())
}

// TestCommandType_CycleOnlyMCPPrompts covers the case with MCP prompts but
// no custom commands: UserCommands must be skipped entirely by both
// directions.
func TestCommandType_CycleOnlyMCPPrompts(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, []commands.MCPPrompt{{ID: "m1", PromptID: "prompt"}})

	c.selected = SystemCommands
	require.Equal(t, MCPPrompts, c.nextCommandType(), "no user commands: next from System skips straight to MCP")
	c.selected = MCPPrompts
	require.Equal(t, SystemCommands, c.nextCommandType())

	c.selected = SystemCommands
	require.Equal(t, MCPPrompts, c.previousCommandType(), "no user commands: previous from System skips straight to MCP")
	c.selected = MCPPrompts
	require.Equal(t, SystemCommands, c.previousCommandType())
}

// TestCommandType_CycleEmptyCategories covers the case with neither custom
// commands nor MCP prompts: both directions must stay on SystemCommands.
func TestCommandType_CycleEmptyCategories(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)

	c.selected = SystemCommands
	require.Equal(t, SystemCommands, c.nextCommandType())
	require.Equal(t, SystemCommands, c.previousCommandType())

	// UserCommands/MCPPrompts are unreachable via the tab key when both
	// categories are empty, but nextCommandType/previousCommandType must
	// still degrade gracefully (both fall through to SystemCommands) if
	// selected is ever left in one of those states.
	c.selected = UserCommands
	require.Equal(t, SystemCommands, c.nextCommandType())
	require.Equal(t, SystemCommands, c.previousCommandType())

	c.selected = MCPPrompts
	require.Equal(t, SystemCommands, c.nextCommandType())
	require.Equal(t, SystemCommands, c.previousCommandType())
}

// ---- setCommandItems ----

// TestCommands_SetCommandItems verifies it both switches the selected
// category and rebuilds the visible list to match it.
func TestCommands_SetCommandItems(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, []commands.CustomCommand{{ID: "c1", Name: "custom-one"}}, nil)
	require.Equal(t, SystemCommands, c.selected)

	c.setCommandItems(UserCommands)

	require.Equal(t, UserCommands, c.selected)
	require.Len(t, c.list.FilteredItems(), 1)
	item, ok := c.list.FilteredItems()[0].(*CommandItem)
	require.True(t, ok)
	require.Equal(t, "custom_c1", item.ID())
}

// ---- mcpPromptItem ----

// TestMcpPromptItem verifies the built item's identity and content, with
// and without a description.
func TestMcpPromptItem(t *testing.T) {
	t.Parallel()

	s := styles.SennitDark()

	withDesc := mcpPromptItem(&s, commands.MCPPrompt{
		ID: "p1", PromptID: "greet", ClientID: "client-1",
		Title: "Greet", Description: "says hello",
	})
	require.Equal(t, "mcp_p1", withDesc.ID())
	require.Equal(t, "greet", withDesc.Title())
	require.Equal(t, "says hello", withDesc.Description())

	action, ok := withDesc.Action().(ActionRunMCPPrompt)
	require.True(t, ok, "expected ActionRunMCPPrompt, got %T", withDesc.Action())
	require.Equal(t, "greet", action.PromptID)
	require.Equal(t, "client-1", action.ClientID)

	noDesc := mcpPromptItem(&s, commands.MCPPrompt{ID: "p2", PromptID: "bye"})
	require.Empty(t, noDesc.Description())
}

// ---- SetCustomCommands / SetMCPPrompts ----

// TestCommands_SetCustomCommands verifies the stored slice and visible
// list are refreshed only while UserCommands is the active category.
func TestCommands_SetCustomCommands(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)

	// Not on the User tab: the list must not be touched yet, but the
	// stored slice updates so a later tab switch picks it up.
	c.SetCustomCommands([]commands.CustomCommand{{ID: "c1", Name: "one"}})
	require.Len(t, c.customCommands, 1)
	require.Equal(t, SystemCommands, c.selected, "must not switch tabs on its own")

	c.selected = UserCommands
	c.SetCustomCommands([]commands.CustomCommand{{ID: "c1", Name: "one"}, {ID: "c2", Name: "two"}})

	require.Len(t, c.list.FilteredItems(), 2)
	item, ok := c.list.FilteredItems()[1].(*CommandItem)
	require.True(t, ok)
	require.Equal(t, "custom_c2", item.ID())
}

// TestCommands_SetMCPPrompts verifies the stored slice and visible list
// are refreshed only while MCPPrompts is the active category.
func TestCommands_SetMCPPrompts(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)

	c.SetMCPPrompts([]commands.MCPPrompt{{ID: "m1", PromptID: "one"}})
	require.Len(t, c.mcpPrompts, 1)
	require.Equal(t, SystemCommands, c.selected)

	c.selected = MCPPrompts
	c.SetMCPPrompts([]commands.MCPPrompt{{ID: "m1", PromptID: "one"}, {ID: "m2", PromptID: "two"}})

	require.Len(t, c.list.FilteredItems(), 2)
	item, ok := c.list.FilteredItems()[1].(*CommandItem)
	require.True(t, ok)
	require.Equal(t, "mcp_m2", item.ID())
}

// ---- StartLoading / StopLoading ----

// TestCommands_StartStopLoading verifies loading toggles, a Tick command
// is returned once and not while already loading, and StopLoading clears
// the flag.
func TestCommands_StartStopLoading(t *testing.T) {
	t.Parallel()

	c := newTestCommands(t, nil, nil)
	require.False(t, c.loading)

	cmd := c.StartLoading()
	require.True(t, c.loading)
	require.NotNil(t, cmd)
	msg := cmd()
	_, ok := msg.(spinner.TickMsg)
	require.True(t, ok, "expected spinner.TickMsg, got %T", msg)

	require.Nil(t, c.StartLoading(), "must not restart the tick while already loading")

	c.StopLoading()
	require.False(t, c.loading)
}
