package appws

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/configtest"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// modelTestWorkspace builds an AppWorkspace whose coordinator exists (so
// AgentModel does not short-circuit) over a config selecting model.
func modelTestWorkspace(t *testing.T, cfg *config.Config) *AppWorkspace {
	t.Helper()
	store := configtest.NewStore(t, cfg, configtest.WithLoadedPaths(t.TempDir()))
	// AgentCoordinator now lives on App's unexported appServices grouping,
	// so it cannot be named in a composite literal from outside the
	// package; SetAgentCoordinatorForTest is the supported seam.
	a := &app.App{}
	a.SetAgentCoordinatorForTest(&modelStubCoordinator{})
	return NewAppWorkspace(a, store)
}

// modelStubCoordinator is an agent.Coordinator that only answers Model.
// It embeds the interface (nil) so every other method would panic if this
// ever started calling one — which is the point: AgentModel must not need
// the coordinator when the config selects a model.
type modelStubCoordinator struct {
	agent.Coordinator
}

func (c *modelStubCoordinator) Model() agent.Model {
	return agent.Model{CatalogCfg: catwalk.Model{ID: "seeded", Name: "Seeded"}}
}

// The model shown must be the one the next turn will run on, and that is
// the one the config selects: coordinator.run resolves its runtime from
// the config on every dispatch, while the coordinator's own Model() is a
// copy only UpdateModels writes. When those diverged, the display sat on
// the previous model while every answer came from the new one.
func TestAgentModel_ReportsTheConfigSelection(t *testing.T) {
	cfg := &config.Config{
		Model: config.SelectedModel{Provider: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "low"},
	}
	ws := modelTestWorkspace(t, cfg)

	got := ws.AgentModel()
	require.Equal(t, "codex", got.ModelCfg.Provider)
	require.Equal(t, "gpt-5.6-sol", got.ModelCfg.Model)
	require.Equal(t, "low", got.ModelCfg.ReasoningEffort)
}

// A selection the catalog cannot resolve is still named by its id. The
// sidebar exists to say which model this is, and the id says it better
// than a blank line.
func TestAgentModel_NamesAnUnresolvableModelByID(t *testing.T) {
	cfg := &config.Config{
		Model: config.SelectedModel{Provider: "gone", Model: "some-model"},
	}
	ws := modelTestWorkspace(t, cfg)

	got := ws.AgentModel()
	require.Equal(t, "some-model", got.CatalogCfg.ID)
	require.Equal(t, "some-model", got.CatalogCfg.Name)
}

// Before onboarding picks a model the config selects nothing, and the
// coordinator's own view is all there is.
func TestAgentModel_FallsBackToTheCoordinatorWithNoSelection(t *testing.T) {
	ws := modelTestWorkspace(t, &config.Config{})

	got := ws.AgentModel()
	require.Equal(t, "seeded", got.CatalogCfg.ID,
		"with nothing selected the coordinator's own model is the only answer")
}

// An App that never had a coordinator installed (an unconfigured project,
// or one still bootstrapping) must report "not ready" rather than panic —
// every AgentXxx method on AppWorkspace nil-checks app.Coordinator() before
// calling into it.
func TestAgentIsReady_FalseWithNoCoordinatorInstalled(t *testing.T) {
	store := configtest.NewStore(t, &config.Config{}, configtest.WithLoadedPaths(t.TempDir()))
	a := &app.App{}
	ws := NewAppWorkspace(a, store)

	require.False(t, ws.AgentIsReady())
	require.ErrorIs(t, ws.AgentReadyErr(), workspace.ErrAgentNotInitialized)
	require.False(t, ws.AgentIsBusy())
	require.False(t, ws.AgentIsSessionBusy("sess-1"))
	require.Equal(t, 0, ws.AgentQueuedPrompts("sess-1"))
	require.Nil(t, ws.AgentQueuedPromptsList("sess-1"))
}

// SetLiveSession is inert: AgentRunStream reports the run's session
// through App.ReportCurrentSession, and this double only has to answer it.
func (c *modelStubCoordinator) SetLiveSession(string) {}
