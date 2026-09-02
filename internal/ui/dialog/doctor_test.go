package dialog

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	providerruntime "github.com/rave-soft/sennit/internal/providers/runtime"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// doctorTestWorkspace is a minimal [workspace.Workspace] stub covering what
// DoctorProblems reads: Config() and MCPGetStates().
type doctorTestWorkspace struct {
	workspace.Workspace
	cfg    *config.Config
	states map[string]workspace.MCPClientInfo
}

// The three below mirror what the dialog used to run for itself, so these
// tests still exercise the real diagnostic and the real catalog against
// their own config. SkillStates is empty because no test here has a skill
// catalog to report on.
func (w doctorTestWorkspace) ConfigProblems() []config.Problem  { return config.Doctor(w.cfg) }
func (w doctorTestWorkspace) SkillStates() []*skills.SkillState { return nil }
func (w doctorTestWorkspace) KnownProviders() []catwalk.Provider {
	return providerruntime.Providers(w.cfg)
}

func (w *doctorTestWorkspace) SupportsThreads() bool { return false }

func (w *doctorTestWorkspace) Config() *config.Config { return w.cfg }

func (w *doctorTestWorkspace) MCPGetStates() map[string]workspace.MCPClientInfo { return w.states }

func (w *doctorTestWorkspace) Stats(context.Context, stats.Request) (stats.Snapshot, error) {
	return stats.Snapshot{}, nil
}

func newDoctorTestCommon(t *testing.T, cfg *config.Config, states map[string]workspace.MCPClientInfo) *common.Common {
	t.Helper()
	s := styles.SennitDark()
	return &common.Common{
		Styles:    &s,
		Workspace: &doctorTestWorkspace{cfg: cfg, states: states},
	}
}

func TestDoctorProblems_StaticConfigCheck(t *testing.T) {
	t.Parallel()

	// DoctorProblems is config.Doctor(cfg) plus MCP state — it reads
	// cfg.Problems as-is rather than recomputing them (that recomputation
	// is validUserAgents' job, exercised end to end in
	// internal/config/doctor_test.go), so a Problem the way SetupAgents
	// would have added it is set directly here.
	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{
				Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
				Message: "agent reviewer: model nope/nope not found — falls back to the main model",
				Hint:    "run 'sennit models' to see available provider/model pairs",
			},
		},
	}

	com := newDoctorTestCommon(t, cfg, nil)
	problems := doctorProblemsWithEnvironment(com, func() []config.Problem { return nil })

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaAgent, problems[0].Area)
	require.Contains(t, problems[0].Message, "falls back to the main model")
}

// TestDoctorProblems_MCPFailedState verifies a failed MCP server (the
// registry's existing state, per domain/agent/tools/mcp) is merged in
// alongside the static config.Doctor findings.
func TestDoctorProblems_MCPFailedState(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	states := map[string]workspace.MCPClientInfo{
		"github": {Name: "github", State: workspace.MCPStateError, Error: errors.New("connection refused")},
		"docs":   {Name: "docs", State: workspace.MCPStateConnected},
	}

	com := newDoctorTestCommon(t, cfg, states)
	problems := doctorProblemsWithEnvironment(com, func() []config.Problem { return nil })

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaMCP, problems[0].Area)
	require.Equal(t, "github", problems[0].Subject)
	require.Contains(t, problems[0].Message, "connection refused")
}

func TestDoctorProblems_UsesInjectedEnvironmentProblems(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	com := newDoctorTestCommon(t, cfg, nil)
	injected := config.Problem{Severity: config.SeverityWarn, Area: config.AreaEnvironment, Subject: "clipboard", Message: "injected environment problem"}

	problems := doctorProblemsWithEnvironment(com, func() []config.Problem { return []config.Problem{injected} })
	require.Equal(t, []config.Problem{injected}, problems)
}

func TestDoctorProblems_Clean(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	com := newDoctorTestCommon(t, cfg, nil)

	require.Empty(t, doctorProblemsWithEnvironment(com, func() []config.Problem { return nil }))
}

// TestNewDoctor_ListsProblems checks the dialog's item list reflects
// DoctorProblems, so the /doctor command actually shows what's wrong.
func TestNewDoctor_ListsProblems(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{
				Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
				Message: "agent reviewer: model nope/nope not found — falls back to the main model",
				Hint:    "run 'sennit models' to see available provider/model pairs",
			},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)

	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })
	require.Equal(t, DoctorID, d.ID())

	items, _, err := doctorItemsFrom(com, doctorProblemsWithEnvironment(com, func() []config.Problem { return nil }))
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

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{
				Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
				Message: "agent reviewer: model x/y not found — falls back to the main model",
				Hint:    "run 'sennit models' to see available provider/model pairs",
			},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

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

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.IsType(t, ActionClose{}, action)
	require.Equal(t, doctorModeList, d.mode)
}

// TestDoctor_EscFromDetail_GoesBackToList verifies esc on the detail screen
// returns to the list instead of closing the whole dialog.
func TestDoctor_EscFromDetail_GoesBackToList(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

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

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{Severity: config.SeverityWarn, Area: config.AreaProvider, Subject: "local", Message: "provider local has no api_key"},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, doctorModeDetail, d.mode)

	action := d.HandleMsg(tea.KeyPressMsg{Code: 'p', Text: "p"})
	require.Equal(t, ActionOpenDialog{DialogID: ProvidersID}, action)
}

// TestDoctor_ModelDetail_MOpensModels mirrors the provider case for
// area=model.
func TestDoctor_ModelDetail_MOpensModels(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{Severity: config.SeverityError, Area: config.AreaModel, Subject: "openai/ghost", Message: "main model not found"},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, doctorModeDetail, d.mode)

	action := d.HandleMsg(tea.KeyPressMsg{Code: 'm', Text: "m"})
	require.Equal(t, ActionOpenDialog{DialogID: ModelsID}, action)
}

// TestDoctor_AgentDetail_NoFixShortcut verifies agent-area problems (fixed
// by editing a file, not through a dialog) don't react to p/m.
func TestDoctor_AgentDetail_NoFixShortcut(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer", Message: "problem"},
		},
	}
	com := newDoctorTestCommon(t, cfg, nil)
	d := newDoctorWithEnvironment(com, func() []config.Problem { return nil })

	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'p', Text: "p"}))
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'm', Text: "m"}))
	require.Equal(t, doctorModeDetail, d.mode, "unmatched keys must not change screens")
}
