package model

import (
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	uistyles "github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

// TestSkillStatusItemsIncludesBuiltinSkills verifies sidebar skills include
// both runtime-discovered skill states and builtin skills that may not have
// emitted a SkillState event yet.
func TestSkillStatusItemsIncludesBuiltinSkills(t *testing.T) {
	t.Parallel()

	st := uistyles.SennitDark()
	ui := &UI{
		com: &common.Common{Styles: &st},
		integrationsState: integrationsState{
			skillStates: []*skills.SkillState{
				{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
			},
		},
	}

	items := ui.skillStatusItems()
	require.NotEmpty(t, items)

	var hasGoDoc bool
	for _, item := range items {
		if item.title == st.Resource.Name.Render("go-doc") {
			hasGoDoc = true
			break
		}
	}
	require.True(t, hasGoDoc)

	builtinSkills := skills.DiscoverBuiltin()
	require.NotEmpty(t, builtinSkills)

	var hasBuiltin bool
	for _, skill := range builtinSkills {
		if skill.Name == "go-doc" {
			continue
		}
		expected := st.Resource.Name.Render(skill.Name)
		for _, item := range items {
			if item.title == expected {
				hasBuiltin = true
				break
			}
		}
		if hasBuiltin {
			break
		}
	}
	require.True(t, hasBuiltin)
}

// TestSkillStatusItemsDoesNotMutateBuiltinCache covers a regression:
// skillStatusItems used to sort the process-global builtinSkillsCache.skills
// slice in place. It is not parallel — it directly manipulates that shared
// global, which would race with any other test reading it concurrently.
func TestSkillStatusItemsDoesNotMutateBuiltinCache(t *testing.T) {
	builtin := cachedBuiltinSkills()
	require.GreaterOrEqual(t, len(builtin), 2, "need at least two builtin skills for a reversal to be observable")

	// Force a specific, guaranteed out-of-name-order arrangement so a
	// render-path sort is observable, and restore the original slice
	// afterward so later tests see the cache as they expect it.
	scrambled := slices.Clone(builtin)
	slices.Reverse(scrambled)
	// expected is an independent copy, on its own backing array: scrambled
	// itself gets assigned into builtinSkillsCache.skills below, so an
	// in-place sort of the cache would mutate scrambled's backing array
	// too and the comparison would trivially pass either way.
	expected := slices.Clone(scrambled)
	original := slices.Clone(builtinSkillsCache.skills)
	builtinSkillsCache.skills = scrambled
	t.Cleanup(func() { builtinSkillsCache.skills = original })

	st := uistyles.SennitDark()
	ui := &UI{com: &common.Common{Styles: &st}}

	_ = ui.skillStatusItems()

	require.Equal(t, expected, builtinSkillsCache.skills,
		"skillStatusItems must not sort the shared builtin skills cache in place")
}

func TestSkillStatusItemsExcludesDisabledSkills(t *testing.T) {
	t.Parallel()

	st := uistyles.SennitDark()
	ui := &UI{
		com: &common.Common{
			Styles:    &st,
			Workspace: &testWorkspace{cfg: &config.Config{Options: &config.Options{DisabledSkills: []string{"go-doc", "sennit-config"}}}},
		},
		integrationsState: integrationsState{
			skillStates: []*skills.SkillState{
				{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
			},
		},
	}

	items := ui.skillStatusItems()

	for _, item := range items {
		require.NotEqual(t, "go-doc", item.name)
		require.NotEqual(t, "sennit-config", item.name)
	}
}
