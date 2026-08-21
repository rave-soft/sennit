package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManager_NoGlobalMirrorByDefault(t *testing.T) {
	// Not parallel - touches package-level cache.
	prev := GetLatestStates()
	t.Cleanup(func() { SetLatestStates(prev) })

	SetLatestStates(nil)

	mgrA := NewManager(nil, nil, []*SkillState{{Name: "a", State: StateNormal}})
	mgrB := NewManager(nil, nil, []*SkillState{{Name: "b", State: StateNormal}})

	mgrA.PublishStates(mgrA.States())
	mgrB.PublishStates(mgrB.States())

	// Without WithGlobalMirror, the package-level cache must not be
	// touched by manager construction or PublishStates calls.
	require.Nil(t, GetLatestStates(), "package global must remain untouched")
	require.Equal(t, "a", mgrA.States()[0].Name)
	require.Equal(t, "b", mgrB.States()[0].Name)
}

func TestManager_GlobalMirror(t *testing.T) {
	// Not parallel - touches package-level cache.
	prev := GetLatestStates()
	t.Cleanup(func() { SetLatestStates(prev) })

	SetLatestStates(nil)

	mgr := NewManager(nil, nil, []*SkillState{{Name: "x", State: StateNormal}}, WithGlobalMirror())

	got := GetLatestStates()
	require.Len(t, got, 1)
	require.Equal(t, "x", got[0].Name)

	// PublishStates with mirror enabled forwards to the global cache.
	mgr.SetLatestStates([]*SkillState{{Name: "y", State: StateNormal}})
	got = GetLatestStates()
	require.Len(t, got, 1)
	require.Equal(t, "y", got[0].Name)
}

func TestManager_PublishStatesUpdatesCache(t *testing.T) {
	// Not parallel - exercises WithGlobalMirror, which touches the
	// package-level cache.
	prev := GetLatestStates()
	t.Cleanup(func() { SetLatestStates(prev) })

	SetLatestStates(nil)

	mgr := NewManager(nil, nil, []*SkillState{{Name: "old"}}, WithGlobalMirror())
	t.Cleanup(mgr.Shutdown)

	// PublishStates must update every observable snapshot, not just the
	// pubsub subscribers: Manager.States() (read by coordinator.skillStates
	// for sennit_info) and skills.GetLatestStates() (read by the TUI)
	// must reflect the new value.
	mgr.PublishStates([]*SkillState{{Name: "new"}})

	got := mgr.States()
	require.Len(t, got, 1)
	require.Equal(t, "new", got[0].Name)

	cached := GetLatestStates()
	require.Len(t, cached, 1)
	require.Equal(t, "new", cached[0].Name)
}

func TestManager_SubscribeReceivesPublishedStates(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, nil)
	t.Cleanup(mgr.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := mgr.SubscribeEvents(ctx)

	want := []*SkillState{{Name: "k", State: StateNormal}}
	go mgr.PublishStates(want)

	select {
	case ev := <-ch:
		require.Equal(t, "k", ev.Payload.States[0].Name)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for manager event")
	}
}

func TestManager_ConcurrentWorkspacesAreIsolated(t *testing.T) {
	t.Parallel()

	// Two managers without WithGlobalMirror should not see each other's
	// events; this models a top-level workspace and a spawned thread's
	// workspace running concurrently in the same process.
	mgrA := NewManager(nil, nil, nil)
	mgrB := NewManager(nil, nil, nil)
	t.Cleanup(mgrA.Shutdown)
	t.Cleanup(mgrB.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	chA := mgrA.SubscribeEvents(ctx)
	chB := mgrB.SubscribeEvents(ctx)

	go mgrA.PublishStates([]*SkillState{{Name: "from-a"}})

	select {
	case ev := <-chA:
		require.Equal(t, "from-a", ev.Payload.States[0].Name)
	case <-time.After(2 * time.Second):
		t.Fatal("workspace A never received its own event")
	}

	select {
	case ev := <-chB:
		t.Fatalf("workspace B received workspace A's event: %v", ev)
	case <-time.After(100 * time.Millisecond):
		// Expected — B's stream is isolated.
	}
}

func TestDiscoverFromConfig(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "custom-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: custom-skill\ndescription: A custom skill for tests.\n---\nDo a thing.\n"),
		0o644,
	))

	allSkills, activeSkills, states := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths:    []string{tmp},
		DisabledSkills: nil,
	})

	// Builtins plus our one custom skill.
	require.NotEmpty(t, allSkills)
	require.NotEmpty(t, activeSkills)
	require.GreaterOrEqual(t, len(allSkills), 2)
	require.GreaterOrEqual(t, len(activeSkills), 2)

	// The custom skill is present with full Instructions populated, so
	// the coordinator can render system prompts without re-walking the
	// filesystem.
	var custom *Skill
	for _, s := range allSkills {
		if s.Name == "custom-skill" {
			custom = s
			break
		}
	}
	require.NotNil(t, custom)
	require.NotEmpty(t, custom.Instructions, "DiscoverFromConfig must return Skill.Instructions")

	// State snapshot includes the custom skill too.
	foundCustom := false
	for _, s := range states {
		if s.Name == "custom-skill" {
			foundCustom = true
			require.Equal(t, StateNormal, s.State)
		}
	}
	require.True(t, foundCustom, "states slice should include the custom skill")
}

func TestDiscoverFromConfig_DisabledFiltered(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "off-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: off-skill\ndescription: Should be filtered.\n---\nx\n"),
		0o644,
	))

	allSkills, activeSkills, states := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths:    []string{tmp},
		DisabledSkills: []string{"off-skill"},
	})

	// All discovered: yes; active: no.
	hasInAll := false
	for _, s := range allSkills {
		if s.Name == "off-skill" {
			hasInAll = true
		}
	}
	require.True(t, hasInAll, "DisabledSkills must not be removed from allSkills")

	for _, s := range activeSkills {
		require.NotEqual(t, "off-skill", s.Name, "DisabledSkills must be removed from activeSkills")
	}

	// State snapshot still carries discovered entries (UI re-applies filter).
	hasInStates := false
	for _, s := range states {
		if s.Name == "off-skill" {
			hasInStates = true
		}
	}
	require.True(t, hasInStates)
}

func TestDiscoverFromConfig_Resolver(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "envvar-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: envvar-skill\ndescription: Env-resolved.\n---\nx\n"),
		0o644,
	))

	allSkills, _, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{"$CUSTOM_SKILLS_DIR"},
		Resolver: func(s string) (string, error) {
			if s == "$CUSTOM_SKILLS_DIR" {
				return tmp, nil
			}
			return s, errors.New("unknown")
		},
	})

	found := false
	for _, s := range allSkills {
		if s.Name == "envvar-skill" {
			found = true
		}
	}
	require.True(t, found, "DiscoverFromConfig must expand $VAR via Resolver")
}

// A thread's worktree carries no .sennit/skills of its own, so the parent
// hands its skills down. They must arrive as fully usable skills — with
// Instructions, since that is what the agent's prompt renders — and be
// visible in the state snapshot the skills UI reads.
func TestDiscoverFromConfig_InheritedSkills(t *testing.T) {
	t.Parallel()

	inherited := &Skill{
		Name:          "parent-skill",
		Description:   "Handed down by the parent workspace.",
		Instructions:  "Do the parent's thing.",
		SkillFilePath: "/parent/.sennit/skills/parent-skill/" + SkillFileName,
	}

	allSkills, activeSkills, states := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths:     []string{t.TempDir()},
		InheritedSkills: []*Skill{inherited},
	})

	require.NotNil(t, findSkill(allSkills, "parent-skill"))
	got := findSkill(activeSkills, "parent-skill")
	require.NotNil(t, got, "an inherited skill must be active by default")
	require.Equal(t, "Do the parent's thing.", got.Instructions)

	var state *SkillState
	for _, s := range states {
		if s.Name == "parent-skill" {
			state = s
		}
	}
	require.NotNil(t, state, "an inherited skill must appear in the state snapshot")
	require.Equal(t, StateNormal, state.State)
}

// The workspace's own SKILL.md wins over one of the same name handed down,
// mirroring how a child workspace's agents override inherited agents.
func TestDiscoverFromConfig_OwnSkillOverridesInherited(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "shared-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: shared-skill\ndescription: The workspace's own.\n---\nLocal instructions.\n"),
		0o644,
	))

	_, activeSkills, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{tmp},
		InheritedSkills: []*Skill{{
			Name:         "shared-skill",
			Description:  "The parent's.",
			Instructions: "Inherited instructions.",
		}},
	})

	got := findSkill(activeSkills, "shared-skill")
	require.NotNil(t, got)
	require.Equal(t, "Local instructions.", got.Instructions)
}

// DisabledSkills is the child workspace's own setting and must apply to
// what it inherited, or a thread could not turn a parent skill off.
func TestDiscoverFromConfig_InheritedSkillCanBeDisabled(t *testing.T) {
	t.Parallel()

	_, activeSkills, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths:     []string{t.TempDir()},
		DisabledSkills:  []string{"parent-skill"},
		InheritedSkills: []*Skill{{Name: "parent-skill", Description: "Handed down."}},
	})

	require.Nil(t, findSkill(activeSkills, "parent-skill"))
}

// Builtins are discovered by every workspace from the same embedded FS, so
// handing them down would only duplicate them.
func TestInheritableDropsBuiltins(t *testing.T) {
	t.Parallel()

	got := Inheritable([]*Skill{
		{Name: "builtin-one", Builtin: true},
		{Name: "project-one"},
		nil,
	})

	require.Len(t, got, 1)
	require.Equal(t, "project-one", got[0].Name)
}

func findSkill(list []*Skill, name string) *Skill {
	for _, s := range list {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// A skill is loaded by reading the location the catalog advertises. For an
// inherited skill that location must not be the parent's file — a thread
// may not read outside its worktree — so it is rewritten to an
// InheritedPrefix address the read tool serves from the skill's Source.
func TestInheritableRewritesLocationAndCarriesSource(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, "parent-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	source := "---\nname: parent-skill\ndescription: The parent's skill.\n---\nParent instructions.\n"
	skillPath := filepath.Join(skillDir, SkillFileName)
	require.NoError(t, os.WriteFile(skillPath, []byte(source), 0o644))

	parsed, err := Parse(skillPath)
	require.NoError(t, err)
	require.Equal(t, source, parsed.Source, "Parse must keep the text so it can travel")

	inherited := Inheritable([]*Skill{parsed})
	require.Len(t, inherited, 1)
	got := inherited[0]

	require.Equal(t, InheritedPrefix+"parent-skill/"+SkillFileName, got.SkillFilePath)
	require.NotContains(t, got.SkillFilePath, tmp, "the parent's path must not travel to the child")
	require.Equal(t, skillPath, parsed.SkillFilePath, "the parent's own catalog must keep the real path")

	// The rewritten address resolves back to the original text.
	src, ok := NewTracker(inherited).InheritedSource(got.SkillFilePath)
	require.True(t, ok)
	require.Equal(t, source, src)

	// And that text is a valid SKILL.md, which is what the read tool
	// parses to report the resource it just loaded.
	reparsed, err := ParseContent([]byte(src))
	require.NoError(t, err)
	require.Equal(t, "parent-skill", reparsed.Name)
	require.Equal(t, "Parent instructions.", reparsed.Instructions)
}

// The skills viewer opens a skill by the same location, so it must not go
// to disk for an inherited one either.
func TestReadContentServesInheritedFromMemory(t *testing.T) {
	t.Parallel()

	source := "---\nname: parent-skill\ndescription: The parent's skill.\n---\nParent instructions.\n"
	inherited := &Skill{
		Name:          "parent-skill",
		Description:   "The parent's skill.",
		Instructions:  "Parent instructions.",
		Source:        source,
		SkillFilePath: InheritedPrefix + "parent-skill/" + SkillFileName,
	}

	content, result, err := ReadContent([]*Skill{inherited}, nil, "", inherited.SkillFilePath)
	require.NoError(t, err)
	require.Equal(t, source, string(content))
	require.Equal(t, "parent-skill", result.Name)
}
