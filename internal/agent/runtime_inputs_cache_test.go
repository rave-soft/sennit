package agent

import (
	"testing"

	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// TestRuntimeInputsCacheReflectsSkillsRefresh guards runtimeInputs()'s
// memoization: a RefreshSkills call must be visible on the very next
// runtimeInputs() call, not held back until some unrelated cache key
// changes. Both signals it depends on for skills (skillsGen) and
// delegation tools (the delegationTools pointer) are exercised here so a
// future change to either invalidation path is caught.
func TestRuntimeInputsCacheReflectsSkillsRefresh(t *testing.T) {
	before := []*skills.Skill{{Name: "before"}}
	after := []*skills.Skill{{Name: "after"}}

	d := &delegationFinalizer{
		agentDeps:    &agentDeps{},
		builder:      &runtimeBuilder{agentDeps: &agentDeps{}},
		allSkills:    before,
		activeSkills: before,
		skillTracker: skills.NewTracker(before),
	}

	got := d.runtimeInputs()
	require.Len(t, got.allSkills, 1)
	require.Equal(t, "before", got.allSkills[0].Name)

	d.RefreshSkills(after, after)

	got = d.runtimeInputs()
	require.Len(t, got.allSkills, 1, "runtimeInputs() must reflect the RefreshSkills call on its very next call")
	require.Equal(t, "after", got.allSkills[0].Name)
}

// TestRuntimeInputsCacheReflectsSetDelegationTools mirrors the skills case
// for the thread/task adapter pair: a SetDelegationTools call must be
// visible on the next runtimeInputs() call even though it does not change
// the config version or the skills generation the cache also keys on.
func TestRuntimeInputsCacheReflectsSetDelegationTools(t *testing.T) {
	d := &delegationFinalizer{agentDeps: &agentDeps{}, builder: &runtimeBuilder{agentDeps: &agentDeps{}}}

	got := d.runtimeInputs()
	require.Nil(t, got.delegationTools.threads)

	threads := noopThreadManager{}
	d.SetDelegationTools(threads, nil)

	got = d.runtimeInputs()
	require.Equal(t, tools.ThreadManager(threads), got.delegationTools.threads,
		"runtimeInputs() must reflect the SetDelegationTools call on its very next call")
}
