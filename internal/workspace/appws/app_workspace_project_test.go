package appws

import (
	"errors"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestAppWorkspace_SkillStatesReadsManager pins SkillStates to the
// workspace's own *skills.Manager rather than any process-wide cache: two
// workspaces backed by different managers must each report their own
// manager's current states, and a later PublishStates on one manager must
// be visible immediately without any extra plumbing.
func TestAppWorkspace_SkillStatesReadsManager(t *testing.T) {
	t.Parallel()

	mgrA := skills.NewManager(nil, nil, []*skills.SkillState{{Name: "a", State: skills.StateNormal}})
	t.Cleanup(mgrA.Shutdown)
	mgrB := skills.NewManager(nil, nil, []*skills.SkillState{{Name: "b", State: skills.StateNormal}})
	t.Cleanup(mgrB.Shutdown)

	appA := &app.App{}
	appA.Skills = mgrA
	appB := &app.App{}
	appB.Skills = mgrB
	wsA := &AppWorkspace{app: appA}
	wsB := &AppWorkspace{app: appB}

	require.Equal(t, []*skills.SkillState{{Name: "a", State: skills.StateNormal}}, wsA.SkillStates())
	require.Equal(t, []*skills.SkillState{{Name: "b", State: skills.StateNormal}}, wsB.SkillStates())

	mgrA.PublishStates([]*skills.SkillState{{Name: "a-updated", State: skills.StateNormal}})
	require.Equal(t, []*skills.SkillState{{Name: "a-updated", State: skills.StateNormal}}, wsA.SkillStates())
	// The other workspace's manager is unaffected.
	require.Equal(t, []*skills.SkillState{{Name: "b", State: skills.StateNormal}}, wsB.SkillStates())
}

// newProjectTestWorkspace builds an AppWorkspace around cfg without reading
// any config file (see config.NewStore), and a skills manager reporting
// skillStates, for testing DoctorProblems' merge without a real project on
// disk.
func newProjectTestWorkspace(cfg *config.Config, skillStates []*skills.SkillState) *AppWorkspace {
	store := config.NewStore(config.StoreOptions{Config: cfg})
	mgr := skills.NewManager(nil, nil, skillStates)
	a := &app.App{}
	a.Skills = mgr
	// DoctorProblems reaches app.MCP.GetStates(), which dereferences the
	// registry's internal map — a zero App leaves MCP nil, so give it a
	// real, empty registry rather than special-casing a nil receiver.
	a.MCP = mcp.NewRegistry()
	return &AppWorkspace{app: a, store: store}
}

// TestAppWorkspace_DoctorProblems_MergesEveryKind pins the merge order and
// content: ConfigProblems' static findings, the injected environment
// probe, and skill problems all end up in the one list DoctorProblems
// (via doctorProblemsWithEnvironment) returns.
func TestAppWorkspace_DoctorProblems_MergesEveryKind(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMap[string, config.ProviderConfig](),
		Problems: []config.Problem{
			{
				Severity: config.SeverityWarn, Area: config.AreaAgent, Subject: "reviewer",
				Message: "agent reviewer: model nope/nope not found — falls back to the main model",
			},
		},
	}
	w := newProjectTestWorkspace(cfg, nil)
	injected := config.Problem{Severity: config.SeverityWarn, Area: config.AreaEnvironment, Subject: "clipboard", Message: "injected environment problem"}

	problems := w.doctorProblemsWithEnvironment(func() []config.Problem { return []config.Problem{injected} })

	require.Len(t, problems, 2)
	require.Equal(t, config.AreaAgent, problems[0].Area)
	require.Equal(t, injected, problems[1])
}

// TestAppWorkspace_DoctorProblems_Clean verifies a workspace with nothing
// wrong reports no problems at all.
func TestAppWorkspace_DoctorProblems_Clean(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Options: &config.Options{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	w := newProjectTestWorkspace(cfg, nil)

	require.Empty(t, w.doctorProblemsWithEnvironment(func() []config.Problem { return nil }))
}

// TestMCPDoctorProblems_ErrorAndNeedsAuthOnly proves mcpDoctorProblems
// merges only servers stuck in error/needs-auth, sorted by name for stable
// rendering, and folds the underlying error into the message.
func TestMCPDoctorProblems_ErrorAndNeedsAuthOnly(t *testing.T) {
	t.Parallel()

	states := map[string]workspace.MCPClientInfo{
		"github": {Name: "github", State: workspace.MCPStateError, Error: errors.New("connection refused")},
		"docs":   {Name: "docs", State: workspace.MCPStateConnected},
		"auth":   {Name: "auth", State: workspace.MCPStateNeedsAuth},
	}

	problems := mcpDoctorProblems(states)

	require.Len(t, problems, 2)
	require.Equal(t, "auth", problems[0].Subject)
	require.Equal(t, "github", problems[1].Subject)
	require.Contains(t, problems[1].Message, "connection refused")
}
