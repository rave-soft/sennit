package dialog

import (
	"errors"
	"testing"

	mcptools "github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/ui/common"
	"github.com/rave-soft/braid/internal/ui/styles"
	"github.com/rave-soft/braid/internal/workspace"
	"github.com/stretchr/testify/require"
)

// doctorTestWorkspace is a minimal [workspace.Workspace] stub covering what
// DoctorProblems reads: Config() and MCPGetStates().
type doctorTestWorkspace struct {
	workspace.Workspace
	cfg    *config.Config
	states map[string]mcptools.ClientInfo
}

func (w *doctorTestWorkspace) Config() *config.Config { return w.cfg }

func (w *doctorTestWorkspace) MCPGetStates() map[string]mcptools.ClientInfo { return w.states }

func newDoctorTestCommon(t *testing.T, cfg *config.Config, states map[string]mcptools.ClientInfo) *common.Common {
	t.Helper()
	s := styles.CharmtonePantera()
	return &common.Common{
		Styles:    &s,
		Workspace: &doctorTestWorkspace{cfg: cfg, states: states},
	}
}

func TestDoctorProblems_StaticConfigCheck(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents: map[string]config.Agent{
			"reviewer": {Prompt: "review code", Model: "nope/nope"},
		},
	}
	cfg.SetupAgents()

	com := newDoctorTestCommon(t, cfg, nil)
	problems := DoctorProblems(com)

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaAgent, problems[0].Area)
	require.Contains(t, problems[0].Message, "falls back to the main model")
}

// TestDoctorProblems_MCPFailedState verifies a failed MCP server (the
// registry's existing state, per internal/agent/tools/mcp) is merged in
// alongside the static config.Doctor findings.
func TestDoctorProblems_MCPFailedState(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	states := map[string]mcptools.ClientInfo{
		"github": {Name: "github", State: mcptools.StateError, Error: errors.New("connection refused")},
		"docs":   {Name: "docs", State: mcptools.StateConnected},
	}

	com := newDoctorTestCommon(t, cfg, states)
	problems := DoctorProblems(com)

	require.Len(t, problems, 1)
	require.Equal(t, config.AreaMCP, problems[0].Area)
	require.Equal(t, "github", problems[0].Subject)
	require.Contains(t, problems[0].Message, "connection refused")
}

func TestDoctorProblems_Clean(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	com := newDoctorTestCommon(t, cfg, nil)

	require.Empty(t, DoctorProblems(com))
}

// TestNewDoctor_ListsProblems checks the dialog's item list reflects
// DoctorProblems, so the /doctor command actually shows what's wrong.
func TestNewDoctor_ListsProblems(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Agents: map[string]config.Agent{
			"reviewer": {Prompt: "review code", Model: "nope/nope"},
		},
	}
	cfg.SetupAgents()
	com := newDoctorTestCommon(t, cfg, nil)

	d := NewDoctor(com)
	require.Equal(t, DoctorID, d.ID())

	items, _, err := doctorItems(com)
	require.NoError(t, err)
	require.Len(t, items, 1)

	rendered := items[0].Render(80)
	require.Contains(t, rendered, "reviewer")
}
