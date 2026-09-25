package model

import (
	"slices"
	"testing"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/ui/common"
	uistyles "github.com/rave-soft/sennit/internal/ui/styles"
	"github.com/rave-soft/sennit/internal/workspace"
	"github.com/stretchr/testify/require"
)

// TestSkillStatusItemsIncludesBuiltinSkills verifies sidebar skills include
// both runtime-discovered skill states and builtin skills that may not have
// emitted a SkillState event yet.
func TestSkillStatusItemsIncludesBuiltinSkills(t *testing.T) {
	t.Parallel()

	builtinSkills := skills.DiscoverBuiltin()
	require.NotEmpty(t, builtinSkills)

	st := uistyles.SennitDark()
	ui := &UI{
		com: &common.Common{Styles: &st},
		integrationsState: integrationsState{
			skillStates: []*skills.SkillState{
				{Name: "go-doc", Path: "/tmp/go-doc/SKILL.md", State: skills.StateNormal},
			},
			// builtinSkills is populated by loadBuiltinSkillsCmd (Init) in
			// the real UI; set directly here since this test drives
			// skillStatusItems without going through Init.
			builtinSkills: builtinSkills,
		},
	}

	items := ui.skillStatusItems(ui.com)
	require.NotEmpty(t, items)

	var hasGoDoc bool
	for _, item := range items {
		if item.title == st.Resource.Name.Render("go-doc") {
			hasGoDoc = true
			break
		}
	}
	require.True(t, hasGoDoc)

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

// builtinSkillsWorkspace answers BuiltinSkills with what the binary
// ships, which is what loadBuiltinSkillsCmd reads off-thread.
type builtinSkillsWorkspace struct {
	workspace.Workspace
}

func (builtinSkillsWorkspace) BuiltinSkills() []*skills.Skill { return skills.DiscoverBuiltin() }

// TestLoadBuiltinSkillsCmd_RunsOffThread pins the fix for the offender the
// update-goroutine guard found: BuiltinSkills used to be read synchronously
// from cachedBuiltinSkills on a render path (skillStatusItems). It must now
// only be read inside loadBuiltinSkillsCmd's returned closure, and land on
// integrationsState.builtinSkills once that closure runs.
func TestLoadBuiltinSkillsCmd_RunsOffThread(t *testing.T) {
	t.Parallel()

	st := uistyles.SennitDark()
	com := &common.Common{Styles: &st, Workspace: builtinSkillsWorkspace{}}
	ui := &UI{com: com}

	cmd := loadBuiltinSkillsCmd(com, ui)
	require.NotNil(t, cmd)
	require.Empty(t, ui.builtinSkills, "must not be populated before the cmd runs")

	msg, ok := cmd().(builtinSkillsLoadedMsg)
	require.True(t, ok)
	require.NotEmpty(t, msg.skills)
}

// TestSkillStatusItemsDoesNotMutateBuiltinCache covers a regression:
// skillStatusItems used to sort the shared builtin-skills slice in place.
// Since that slice can be backed by the process-global builtinSkillsCache
// (see cachedBuiltinSkills, still shared across every UI instance's
// loadBuiltinSkillsCmd), an in-place sort here would corrupt what every
// other instance sees too.
func TestSkillStatusItemsDoesNotMutateBuiltinCache(t *testing.T) {
	t.Parallel()

	builtin := skills.DiscoverBuiltin()
	require.GreaterOrEqual(t, len(builtin), 2, "need at least two builtin skills for a reversal to be observable")

	// Force a specific, guaranteed out-of-name-order arrangement so a
	// render-path sort is observable.
	scrambled := slices.Clone(builtin)
	slices.Reverse(scrambled)
	// expected is an independent copy, on its own backing array: scrambled
	// itself is assigned into the UI's builtinSkills field below, so an
	// in-place sort of that field would mutate scrambled's backing array
	// too and the comparison would trivially pass either way.
	expected := slices.Clone(scrambled)

	st := uistyles.SennitDark()
	ui := &UI{
		com:               &common.Common{Styles: &st},
		integrationsState: integrationsState{builtinSkills: scrambled},
	}

	_ = ui.skillStatusItems(ui.com)

	require.Equal(t, expected, ui.builtinSkills,
		"skillStatusItems must not sort the shared builtin skills slice in place")
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

	items := ui.skillStatusItems(ui.com)

	for _, item := range items {
		require.NotEqual(t, "go-doc", item.name)
		require.NotEqual(t, "sennit-config", item.name)
	}
}
