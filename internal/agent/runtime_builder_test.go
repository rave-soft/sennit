package agent

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// TestRuntimeConfigSnapshot_ImplementsSkillsProvider pins the wiring the
// prompt's SkillsProvider hook exists for. Without a store that supplies
// the coordinator's list, promptData falls back to walking the configured
// skill directories — which is how a thread ended up with no project
// skills in its prompt while sennit_info, reading the coordinator's list,
// reported them active: a thread's worktree has no .sennit/skills of its
// own, and inheritance is what covers that.
func TestRuntimeConfigSnapshot_ImplementsSkillsProvider(t *testing.T) {
	t.Parallel()

	var snapshot any = runtimeConfigSnapshot{
		activeSkills: []*skills.Skill{{Name: "inherited-one"}},
	}
	provider, ok := snapshot.(prompt.SkillsProvider)
	require.True(t, ok, "the prompt falls back to disk discovery unless the store offers its list")

	active := provider.ActiveSkills()
	require.Len(t, active, 1)
	require.Equal(t, "inherited-one", active[0].Name)
}
