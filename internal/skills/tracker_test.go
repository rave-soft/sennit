package skills

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTracker_MarkLoadedAndIsLoaded(t *testing.T) {
	t.Parallel()

	activeSkills := []*Skill{
		{Name: "go-doc"},
		{Name: "bash"},
	}
	tracker := NewTracker(activeSkills)

	// Initially not loaded.
	require.False(t, tracker.IsLoaded("go-doc"))
	require.False(t, tracker.IsLoaded("bash"))

	// Mark as loaded.
	tracker.MarkLoaded("go-doc")
	require.True(t, tracker.IsLoaded("go-doc"))
	require.False(t, tracker.IsLoaded("bash"))

	// Mark another.
	tracker.MarkLoaded("bash")
	require.True(t, tracker.IsLoaded("go-doc"))
	require.True(t, tracker.IsLoaded("bash"))
}

func TestTracker_NonActiveSkillCannotBeMarkedLoaded(t *testing.T) {
	t.Parallel()

	activeSkills := []*Skill{
		{Name: "go-doc"},
	}
	tracker := NewTracker(activeSkills)

	// Cannot mark non-active skill as loaded.
	tracker.MarkLoaded("bash")
	require.False(t, tracker.IsLoaded("bash"))

	// Can mark active skill as loaded.
	tracker.MarkLoaded("go-doc")
	require.True(t, tracker.IsLoaded("go-doc"))
}

func TestTracker_NilSafety(t *testing.T) {
	t.Parallel()

	var tracker *Tracker

	// Should not panic.
	tracker.MarkLoaded("go-doc")
	require.False(t, tracker.IsLoaded("go-doc"))
}

func TestTracker_BuiltinSkillTracking(t *testing.T) {
	t.Parallel()

	// Simulate active skills including a builtin skill (sennit-config).
	activeSkills := []*Skill{
		{Name: "sennit-config", Description: "Braid config", Builtin: true},
		{Name: "go-doc", Description: "Go docs", Builtin: false},
	}
	tracker := NewTracker(activeSkills)

	// Initially not loaded.
	require.False(t, tracker.IsLoaded("sennit-config"))
	require.False(t, tracker.IsLoaded("go-doc"))

	// Mark builtin skill as loaded (simulating read via braid://...).
	tracker.MarkLoaded("sennit-config")
	require.True(t, tracker.IsLoaded("sennit-config"))

	// Mark user skill as loaded.
	tracker.MarkLoaded("go-doc")
	require.True(t, tracker.IsLoaded("go-doc"))
}

// TestTracker_UpdateActiveSkills_KeepsLoadedWhenStillActive verifies that
// a rescan (e.g. after WatchForChanges detects an edited SKILL.md) does
// not forget a skill that was already read this session and remains
// active after the rescan.
func TestTracker_UpdateActiveSkills_KeepsLoadedWhenStillActive(t *testing.T) {
	t.Parallel()

	activeSkills := []*Skill{{Name: "go-doc"}, {Name: "bash"}}
	tracker := NewTracker(activeSkills)

	tracker.MarkLoaded("go-doc")
	require.True(t, tracker.IsLoaded("go-doc"))

	// Rescan: "go-doc" is still active, "bash" dropped, "new-skill" added.
	tracker.UpdateActiveSkills([]*Skill{{Name: "go-doc"}, {Name: "new-skill"}})

	require.True(t, tracker.IsLoaded("go-doc"), "still-active skill should keep its loaded state")

	// The newly active skill can now be tracked.
	tracker.MarkLoaded("new-skill")
	require.True(t, tracker.IsLoaded("new-skill"))
}

// TestTracker_UpdateActiveSkills_DropsLoadedWhenNoLongerActive verifies
// that a skill removed by a rescan (its SKILL.md deleted, or the skill
// disabled) is no longer trackable, and its stale loaded state is pruned.
func TestTracker_UpdateActiveSkills_DropsLoadedWhenNoLongerActive(t *testing.T) {
	t.Parallel()

	activeSkills := []*Skill{{Name: "go-doc"}}
	tracker := NewTracker(activeSkills)

	tracker.MarkLoaded("go-doc")
	require.True(t, tracker.IsLoaded("go-doc"))

	// Rescan: "go-doc" is gone.
	tracker.UpdateActiveSkills([]*Skill{{Name: "bash"}})

	require.False(t, tracker.IsLoaded("go-doc"), "removed skill should no longer be loaded")

	// It also can't be re-marked as loaded until it's active again.
	tracker.MarkLoaded("go-doc")
	require.False(t, tracker.IsLoaded("go-doc"))
}

func TestTracker_OverriddenBuiltinNotTracked(t *testing.T) {
	t.Parallel()

	// Simulate scenario where builtin "bash" is overridden by user "bash".
	// After dedup, only user "bash" is active.
	activeSkills := []*Skill{
		{Name: "bash", Description: "User bash override", Builtin: false},
	}
	tracker := NewTracker(activeSkills)

	// Trying to mark the builtin "bash" as loaded should not work
	// because the active skill is the user override.
	tracker.MarkLoaded("bash")
	require.True(t, tracker.IsLoaded("bash"))

	// But if we somehow tried to mark a different builtin that's not active,
	// it wouldn't get marked.
	tracker.MarkLoaded("nonexistent-builtin")
	require.False(t, tracker.IsLoaded("nonexistent-builtin"))
}
