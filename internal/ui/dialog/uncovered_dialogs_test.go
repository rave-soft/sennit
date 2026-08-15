package dialog

import (
	"errors"
	"image"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/rave-soft/braid/internal/commands"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	mcptools "github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

func newUncoveredDialogCommon(t *testing.T) *common.Common {
	t.Helper()
	s := styles.BraidDark()
	return &common.Common{Styles: &s}
}

func TestArguments_KeyboardNavigationValidationAndViewport(t *testing.T) {
	t.Parallel()

	args := make([]commands.Argument, 5)
	for i := range args {
		args[i] = commands.Argument{ID: string(rune('a' + i)), Title: "argument", Required: i == 4}
	}
	dialog := NewArguments(newUncoveredDialogCommon(t), "Run", "", args, ActionRunCustomCommand{})
	for range 4 {
		dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	require.Equal(t, 4, dialog.focused)
	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, warned := action.(ActionCmd)
	require.True(t, warned, "missing required field must return a warning command")
	require.Equal(t, 4, dialog.focused, "validation must not change focus")

	dialog.HandleMsg(keyPress("x"))
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	run, ok := action.(ActionRunCustomCommand)
	require.True(t, ok, "valid final field must submit, got %T", action)
	require.Equal(t, "x", run.Args["e"])
}

func TestAWSSSO_StateTransitionsAndNarrowDraw(t *testing.T) {
	t.Parallel()

	dialog, _ := NewAWSSSO(newUncoveredDialogCommon(t), "aws sso login")
	require.Equal(t, awsSSOStateWaiting, dialog.state)
	dialog.SetURL("https://example.test/verification/this-is-a-deliberately-long-token")
	dialog.Finish("")
	require.Equal(t, awsSSOStateSuccess, dialog.state)
	require.IsType(t, ActionClose{}, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))

	dialog.SetURL("https://stale.example.test")
	require.NotEqual(t, "https://stale.example.test", dialog.url)
	dialog.Finish("authorization timed out\nplease retry")
	require.Equal(t, awsSSOStateError, dialog.state)
	require.Contains(t, dialog.innerContent(), "authorization timed out please retry")

	area := image.Rect(0, 0, 8, 4)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { dialog.Draw(scr, area) })
	require.LessOrEqual(t, dialog.Width(), area.Dx())
}

func TestMCPAuth_MultiServerStateTransitions(t *testing.T) {
	t.Parallel()

	dialog, _ := NewMCPAuth(newUncoveredDialogCommon(t), []mcptools.MCPPendingAuthServer{
		{Name: "first", URL: "https://first.example.test"},
		{Name: "second", URL: "https://second.example.test"},
	}, func(name string) string { return "https://auth.example.test/" + name })

	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	command, ok := action.(ActionCmd)
	require.True(t, ok, "enter in prompt state must start authentication")
	require.NotNil(t, command.Cmd)
	require.Equal(t, MCPAuthStateAuthenticating, dialog.state)
	require.NotNil(t, dialog.cancelAuth)

	dialog.HandleMsg(ActionMCPAuthComplete{Name: "first"})
	require.Equal(t, MCPAuthStateSuccess, dialog.state)
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "success should advance to the next server")
	require.Equal(t, 1, dialog.current)
	require.Equal(t, MCPAuthStatePrompt, dialog.state)
	dialog.HandleMsg(ActionMCPAuthErrored{Name: "first", Error: errors.New("stale")})
	require.Equal(t, MCPAuthStatePrompt, dialog.state, "stale result must not affect the current server")
	dialog.state = MCPAuthStateAuthenticating
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)
}

func TestReasoning_KeyboardSelectionAndNarrowDraw(t *testing.T) {
	t.Parallel()

	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("test", config.ProviderConfig{
		ID: "test", Models: []catwalk.Model{{ID: "reasoning", ReasoningLevels: []string{"low", "high"}}},
	})
	cfg := &config.Config{
		Model:     config.SelectedModel{Provider: "test", Model: "reasoning", ReasoningEffort: "low"},
		Agents:    map[string]config.Agent{config.AgentCoder: {}},
		Providers: providers,
	}
	com := newModelsTestCommon(t, cfg)
	dialog, err := NewReasoning(com)
	require.NoError(t, err)

	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	action := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	selected, ok := action.(ActionSelectReasoningEffort)
	require.True(t, ok)
	require.Equal(t, "high", selected.Effort)
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	require.Equal(t, "low", dialog.selectedID(), "down from the final item must wrap")

	area := image.Rect(0, 0, 8, 5)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { dialog.Draw(scr, area) })
	require.LessOrEqual(t, dialog.Width(), area.Dx())
}

func TestQuestionForm_MouseTabsAndNarrowLayout(t *testing.T) {
	t.Parallel()

	s := styles.BraidDark()
	form := NewQuestionForm(&s, question.Request{ID: "batch", Questions: []question.Question{
		{ID: "one", Type: question.TypeYesNo, Text: "Should this deliberately long first question be accepted?"},
		{ID: "two", Type: question.TypeSingleChoice, Text: "Choose a deliberately long option", Choices: []question.Choice{{ID: "a", Label: "Alpha"}}},
	}})
	form.SetFocused(true)
	wideArea := image.Rect(0, 0, 80, 12)
	wideScreen := uv.NewScreenBuffer(wideArea.Dx(), wideArea.Dy())
	form.Draw(wideScreen, wideArea)
	_, handled := form.HandleMouseClick(30, 1)
	require.True(t, handled)
	require.Equal(t, 1, form.activeIdx)

	form.switchTab(0)
	area := image.Rect(0, 0, 8, 12)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { form.Draw(scr, area) })
	form.HandleKey(tea.KeyPressMsg{Code: ']'})
	require.Equal(t, 1, form.activeIdx)
	form.HandleKey(tea.KeyPressMsg{Code: ']'})
	require.Equal(t, 2, form.activeIdx, "keyboard navigation must still reach confirm when tabs are collapsed")
}
