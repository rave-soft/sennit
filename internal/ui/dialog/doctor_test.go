package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// doctorTestWorkspace is a minimal [workspace.Workspace] stub: DoctorProblems
// returns whatever problems is set to, and calls is incremented each time
// it's called — so tests can prove NewDoctor doesn't call it synchronously.
type doctorTestWorkspace struct {
	workspace.Workspace
	problems []config.Problem
	calls    int
}

func (w *doctorTestWorkspace) SupportsThreads() bool { return false }

func (w *doctorTestWorkspace) DoctorProblems() []config.Problem {
	w.calls++
	return w.problems
}

func newDoctorTestCommon(t *testing.T, problems []config.Problem) (*common.Common, *doctorTestWorkspace) {
	t.Helper()
	s := styles.SennitDark()
	ws := &doctorTestWorkspace{problems: problems}
	return &common.Common{Styles: &s, Workspace: ws}, ws
}

// newDoctorForTest builds a Doctor and drives its returned tea.Cmd to
// completion, feeding the result back into HandleMsg — the sequence the
// real program loop performs, condensed for tests that don't care about
// the intermediate empty-list state.
func newDoctorForTest(com *common.Common) *Doctor {
	d, cmd := NewDoctor(com)
	d.HandleMsg(cmd())
	return d
}

// TestNewDoctor_PerformsNoIOAtConstruction is the regression test for the
// dialog reaching into internal/doctor (which shells out and walks PATH)
// synchronously from its constructor. NewDoctor must return immediately
// with an empty problem list and a command that fetches the real one; the
// workspace's DoctorProblems must not be called until that command runs.
func TestNewDoctor_PerformsNoIOAtConstruction(t *testing.T) {
	t.Parallel()

	com, ws := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
	})

	d, cmd := NewDoctor(com)
	require.Zero(t, ws.calls, "NewDoctor must not call DoctorProblems synchronously")
	require.Empty(t, d.problems, "the dialog must start with an empty problem list")
	require.NotNil(t, cmd, "NewDoctor must return a command to fetch the problem list")

	msg := cmd()
	loaded, ok := msg.(doctorProblemsLoadedMsg)
	require.True(t, ok, "expected doctorProblemsLoadedMsg, got %#v", msg)
	require.Equal(t, 1, ws.calls)
	require.Equal(t, ws.problems, loaded.problems)

	action := d.HandleMsg(msg)
	require.Nil(t, action)
	require.Equal(t, ws.problems, d.problems, "HandleMsg must populate the dialog once the command's result lands")
}

// TestNewDoctor_ListsProblems checks the dialog's item list reflects
// DoctorProblems once loaded, so the /doctor command actually shows what's
// wrong.
func TestNewDoctor_ListsProblems(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{
			Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
			Message: "agent reviewer: model nope/nope not found — falls back to the main model",
			Hint:    "run 'sennit models' to see available provider/model pairs",
		},
	})

	d := newDoctorForTest(com)
	require.Equal(t, DoctorID, d.ID())

	items, _, err := doctorItemsFrom(com, d.problems)
	require.NoError(t, err)
	require.Len(t, items, 1)

	rendered := items[0].Render(80)
	require.Contains(t, rendered, "reviewer")
}

// TestDoctor_EnterOpensDetail is the regression test for the bug report:
// Enter on a problem used to close the dialog silently (selectDialog's
// onSelect always returned ActionClose). It must now switch to the detail
// screen instead, returning no action (the dialog stays open).
func TestDoctor_EnterOpensDetail(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{
			Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
			Message: "agent reviewer: model x/y not found — falls back to the main model",
			Hint:    "run 'sennit models' to see available provider/model pairs",
		},
	})
	d := newDoctorForTest(com)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Nil(t, action, "Enter must not silently close the dialog anymore")
	require.Equal(t, doctorModeDetail, d.mode)
	require.Equal(t, "reviewer", d.detail.Subject)

	// The detail screen must show the full text, not the list's truncated
	// row.
	require.Contains(t, d.detail.Message, "falls back to the main model")
	require.Contains(t, d.detail.Hint, "sennit models")
}

// TestDoctor_EscFromList_Closes preserves the pre-existing behavior: esc on
// the list screen closes the dialog.
func TestDoctor_EscFromList_Closes(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
	})
	d := newDoctorForTest(com)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)
	require.Equal(t, doctorModeList, d.mode)
}

// TestDoctor_EscFromDetail_GoesBackToList verifies esc on the detail screen
// returns to the list instead of closing the whole dialog.
func TestDoctor_EscFromDetail_GoesBackToList(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
	})
	d := newDoctorForTest(com)

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, doctorModeDetail, d.mode)

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Nil(t, action, "esc from detail must go back, not close")
	require.Equal(t, doctorModeList, d.mode)
}

// TestDoctor_ProviderDetail_POpensProviders verifies the area-specific fix
// shortcut: pressing p on a provider problem's detail screen closes Doctor
// by returning ActionOpenDialog{ProvidersID}.
func TestDoctor_ProviderDetail_POpensProviders(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityWarn, Area: config.AreaProvider, Subject: "local", Message: "provider local has no api_key"},
	})
	d := newDoctorForTest(com)

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, doctorModeDetail, d.mode)

	action := d.HandleMsg(tea.KeyPressMsg{Code: 'p', Text: "p"})
	require.Equal(t, ActionOpenDialog{DialogID: ProvidersID}, action)
}

// TestDoctor_ModelDetail_MOpensModels mirrors the provider case for
// area=model.
func TestDoctor_ModelDetail_MOpensModels(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityError, Area: config.AreaModel, Subject: "openai/ghost", Message: "main model not found"},
	})
	d := newDoctorForTest(com)

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, doctorModeDetail, d.mode)

	action := d.HandleMsg(tea.KeyPressMsg{Code: 'm', Text: "m"})
	require.Equal(t, ActionOpenDialog{DialogID: ModelsID}, action)
}

// TestDoctor_AgentDetail_NoFixShortcut verifies agent-area problems (fixed
// by editing a file, not through a dialog) don't react to p/m.
func TestDoctor_AgentDetail_NoFixShortcut(t *testing.T) {
	t.Parallel()

	com, _ := newDoctorTestCommon(t, []config.Problem{
		{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
	})
	d := newDoctorForTest(com)

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'p', Text: "p"}))
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'm', Text: "m"}))
	require.Equal(t, doctorModeDetail, d.mode, "unmatched keys must not change screens")
}
