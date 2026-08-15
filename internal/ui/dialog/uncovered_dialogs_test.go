package dialog

import (
	"errors"
	"fmt"
	"image"
	"strings"
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

	scrollArgs := make([]commands.Argument, 25)
	for i := range scrollArgs {
		scrollArgs[i] = commands.Argument{ID: string(rune('a' + i%26)), Title: fmt.Sprintf("argument %02d", i)}
	}
	scrollDialog := NewArguments(newUncoveredDialogCommon(t), "Run", "", scrollArgs, ActionRunCustomCommand{})
	for range 24 {
		scrollDialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	require.Equal(t, 24, scrollDialog.focused)
	scrollDialog.HandleMsg(keyPress("x"))
	action = scrollDialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	run, ok = action.(ActionRunCustomCommand)
	require.True(t, ok, "last visible field must submit, got %T", action)
	require.Equal(t, "x", run.Args["y"])

	tallArea := image.Rect(0, 0, 100, 12)
	tallScreen := uv.NewScreenBuffer(tallArea.Dx(), tallArea.Dy())
	require.NotPanics(t, func() { scrollDialog.Draw(tallScreen, tallArea) })
	scrollDialog.viewport.SetYOffset(70)
	require.NotPanics(t, func() { scrollDialog.Draw(tallScreen, tallArea) })
	rendered := tallScreen.String()
	require.Contains(t, rendered, "Argument 24", "focused field must render inside the viewport")
	require.NotContains(t, rendered, "Argument 00", "fields above the viewport must not render")
	require.NotContains(t, rendered, "Argument 19", "fields above the viewport must not render")
	require.NotContains(t, rendered, "Argument 20", "fields above the viewport must not render")
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

	area := image.Rect(0, 0, 40, 12)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { dialog.Draw(scr, area) })
	require.LessOrEqual(t, dialog.Width(), area.Dx())
	rendered := scr.String()
	require.Contains(t, rendered, "Authentication failed")
	require.Contains(t, rendered, "authorization timed out")
	require.Contains(t, rendered, "retry")
	require.True(t, strings.ContainsAny(rendered, "┌╭╔"), "narrow draw must still render the dialog frame")
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
	startedMsg := command.Cmd()
	batch, ok := startedMsg.(tea.BatchMsg)
	require.True(t, ok, "start authentication must return a batch command, got %T", startedMsg)
	var started ActionMCPAuthStarted
	for _, cmd := range batch {
		if s, ok := cmd().(ActionMCPAuthStarted); ok {
			started = s
			break
		}
	}
	require.Equal(t, "first", started.Name)
	require.NotNil(t, started.Ctx)

	dialog.HandleMsg(ActionMCPAuthComplete{Name: "second"})
	require.Equal(t, MCPAuthStateAuthenticating, dialog.state, "stale complete must not affect the current server")
	dialog.HandleMsg(ActionMCPAuthComplete{Name: "first"})
	require.Equal(t, MCPAuthStateSuccess, dialog.state)
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "success should advance to the next server")
	require.Equal(t, 1, dialog.current)
	require.Equal(t, MCPAuthStatePrompt, dialog.state)

	require.IsType(t, ActionClose{}, dialog.HandleMsg(keyPress("s")), "skip must advance to the last server")
	require.Equal(t, 2, dialog.current)
	require.IsType(t, ActionClose{}, dialog.HandleMsg(keyPress("s")), "skipping the last server must close the dialog")

	dialog.current = 1
	dialog.state = MCPAuthStatePrompt
	dialog.HandleMsg(ActionMCPAuthErrored{Name: "first", Error: errors.New("stale")})
	require.Equal(t, MCPAuthStatePrompt, dialog.state, "stale result must not affect the current server")
	dialog.HandleMsg(ActionMCPAuthErrored{Name: "second", Error: errors.New("oauth expired")})
	require.Equal(t, MCPAuthStateError, dialog.state)
	require.Contains(t, dialog.innerContent(), "oauth expired")
	dialog.state = MCPAuthStateAuthenticating
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)
	require.Nil(t, dialog.cancelAuth, "close must cancel the in-flight authentication")

	dialog.state = MCPAuthStatePrompt
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.NotNil(t, action, "last server must start authentication")
	dialog.HandleMsg(ActionMCPAuthComplete{Name: "second"})
	action = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.IsType(t, ActionClose{}, action, "all servers done must close the dialog")
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

	area := image.Rect(0, 0, 12, 8)
	scr := uv.NewScreenBuffer(area.Dx(), area.Dy())
	require.NotPanics(t, func() { dialog.Draw(scr, area) })
	require.LessOrEqual(t, dialog.Width(), area.Dx())
	require.Contains(t, scr.String(), "High")
	require.True(t, strings.ContainsAny(scr.String(), "┌╭╔"), "narrow draw must still render the dialog frame")

	wideArea := image.Rect(0, 0, 60, 12)
	wideScreen := uv.NewScreenBuffer(wideArea.Dx(), wideArea.Dy())
	require.NotPanics(t, func() { dialog.Draw(wideScreen, wideArea) })
	wide := wideScreen.String()
	require.Contains(t, wide, "Select Reasoning Effort")
	require.Contains(t, wide, "High")
	require.Contains(t, wide, "Low")
	require.Contains(t, wide, "current")
}

func TestQuestionForm_MouseTabsAndNarrowLayout(t *testing.T) {
	t.Parallel()

	s := styles.BraidDark()
	form := NewQuestionForm(&s, question.Request{ID: "batch", Questions: []question.Question{
		{ID: "one", Type: question.TypeYesNo, Text: "Should this deliberately long first question be accepted?"},
		{ID: "two", Type: question.TypeSingleChoice, Text: "Choose a deliberately long option", Choices: []question.Choice{{ID: "a", Label: "Alpha"}}},
	}})
	var submitted []question.Answer
	submittedCount := 0
	form.OnAnswer = func(responses []question.Answer) {
		submitted = append([]question.Answer(nil), responses...)
		submittedCount++
	}
	form.SetFocused(true)
	done, _ := form.HandleKey(keyPress("y"))
	require.False(t, done, "answering one question must not submit the batch")
	require.Equal(t, 1, form.activeIdx, "answering must advance to the next tab")
	_, _ = form.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, 2, form.activeIdx, "selecting the choice must land on the confirm tab")
	done, _ = form.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.True(t, done, "confirm tab enter must submit the batch")
	require.Equal(t, 1, submittedCount)
	require.Len(t, submitted, 2)
	require.Equal(t, "one", submitted[0].QuestionID)
	require.NotNil(t, submitted[0].Yes)
	require.True(t, *submitted[0].Yes)
	require.Equal(t, "two", submitted[1].QuestionID)
	require.Equal(t, []string{"a"}, submitted[1].SelectedIDs)
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
