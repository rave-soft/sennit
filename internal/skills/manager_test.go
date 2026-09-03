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

// TestManager_PublishStatesUpdatesState confirms PublishStates updates
// every observable snapshot: Manager.States() (read by
// coordinator.skillStates for sennit_info) as well as pubsub subscribers.
func TestManager_PublishStatesUpdatesState(t *testing.T) {
	t.Parallel()

	mgr := NewManager(nil, nil, []*SkillState{{Name: "old"}})
	t.Cleanup(mgr.Shutdown)

	mgr.PublishStates([]*SkillState{{Name: "new"}})

	got := mgr.States()
	require.Len(t, got, 1)
	require.Equal(t, "new", got[0].Name)
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

	// Two independent managers should not see each other's events; this
	// models a top-level workspace and a spawned thread's workspace
	// running concurrently in the same process.
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

// writeTestSkill writes a minimal SKILL.md for name under dir/name/SKILL.md,
// using description to distinguish same-named skills written to different
// directories in a precedence test.
func writeTestSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: "+name+"\ndescription: "+description+"\n---\nbody\n"),
		0o644,
	))
}

// TestDiscoverFromConfig_LastPathWinsRegardlessOfSort guards the precedence
// rule DiscoverFromConfig relies on: the *last* entry of SkillsPaths wins a
// same-named conflict, independent of how the directory names sort. The two
// directories are named so a lexicographic comparison would pick the wrong
// one — "a-first" sorts before "z-second" — to prove the win comes from
// SkillsPaths order and not from string comparison.
func TestDiscoverFromConfig_LastPathWinsRegardlessOfSort(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	firstDir := filepath.Join(tmp, "z-first")
	secondDir := filepath.Join(tmp, "a-second")
	writeTestSkill(t, firstDir, "shared-skill", "from the first path.")
	writeTestSkill(t, secondDir, "shared-skill", "from the second path.")

	allSkills, _, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{firstDir, secondDir},
	})

	var winner *Skill
	for _, s := range allSkills {
		if s.Name == "shared-skill" {
			winner = s
		}
	}
	require.NotNil(t, winner)
	require.Equal(t, "from the second path.", winner.Description,
		"the later SkillsPaths entry must win regardless of directory name sort order")
}

// TestDiscoverFromConfig_ProjectOverridesGlobalRegardlessOfCheckoutPath
// mirrors how defaults.go orders SkillsPaths: global directories first,
// project directories last. The project directory here sorts before the
// global one lexicographically, which is exactly the case that broke under
// the old cross-path sort (see config.ProjectSkillsDir's doc comment).
func TestDiscoverFromConfig_ProjectOverridesGlobalRegardlessOfCheckoutPath(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	globalDir := filepath.Join(tmp, "zzz-home-config-skills")
	projectDir := filepath.Join(tmp, "aaa-project-skills")
	writeTestSkill(t, globalDir, "release-notes", "the global version.")
	writeTestSkill(t, projectDir, "release-notes", "the project version.")

	allSkills, _, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{globalDir, projectDir},
	})

	var winner *Skill
	for _, s := range allSkills {
		if s.Name == "release-notes" {
			winner = s
		}
	}
	require.NotNil(t, winner)
	require.Equal(t, "the project version.", winner.Description)
}

// TestDiscoverFromConfig_WorkingDirOverridesGitRoot mirrors
// config.ProjectSkillsDir's own order: the git worktree root path first,
// the working directory path last. The working directory here sorts before
// the git root lexicographically, so this fails under a cross-path sort.
func TestDiscoverFromConfig_WorkingDirOverridesGitRoot(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	gitRootDir := filepath.Join(tmp, "zzz-git-root", ".sennit", "skills")
	workingDir := filepath.Join(tmp, "aaa-working-dir", ".sennit", "skills")
	writeTestSkill(t, gitRootDir, "shared-skill", "from the git root.")
	writeTestSkill(t, workingDir, "shared-skill", "from the working directory.")

	allSkills, _, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{gitRootDir, workingDir},
	})

	var winner *Skill
	for _, s := range allSkills {
		if s.Name == "shared-skill" {
			winner = s
		}
	}
	require.NotNil(t, winner)
	require.Equal(t, "from the working directory.", winner.Description)
}

// TestDiscoverFromConfig_UserSkillOverridesBuiltin confirms a user skill
// still wins over a builtin of the same name after the discovery-order fix:
// builtins are discovered first and SkillsPaths entries are discovered
// after them, so Deduplicate's last-occurrence rule keeps the user one.
func TestDiscoverFromConfig_UserSkillOverridesBuiltin(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestSkill(t, tmp, "jq", "the user's own jq skill.")

	allSkills, _, _ := DiscoverFromConfig(DiscoveryConfig{
		SkillsPaths: []string{tmp},
	})

	var winner *Skill
	for _, s := range allSkills {
		if s.Name == "jq" {
			winner = s
		}
	}
	require.NotNil(t, winner)
	require.Equal(t, "the user's own jq skill.", winner.Description)
	require.False(t, winner.Builtin)
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
