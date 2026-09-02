package appws

import (
	"testing"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/skills"
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
