package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testPollInterval is short enough to keep these tests fast while still
// exercising the real poll loop (as opposed to calling scanSkillFiles
// directly), matching how internal/config's watch_test.go drives
// WatchForExternalChanges.
const testPollInterval = 20 * time.Millisecond

// TestWatchForChanges_DetectsAddEditRemove verifies that a SKILL.md file
// added, edited, or removed outside this process (an agent's Write tool,
// or a human editing it directly) is picked up by the poll loop, causing
// mgr.ActiveSkills() to reflect the change without a restart.
func TestWatchForChanges_DetectsAddEditRemove(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg := DiscoveryConfig{SkillsPaths: []string{root}}

	mgr := NewManager(nil, nil, nil)

	notified := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchForChanges(ctx, cfg, mgr, testPollInterval, func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	waitNotified := func(msg string) {
		t.Helper()
		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			t.Fatal(msg)
		}
	}

	// Give the watcher goroutine time to take its first snapshot before
	// any file exists, so the write below is seen as a genuine diff
	// rather than possibly being included in that very first scan.
	time.Sleep(5 * testPollInterval)

	skillDir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	skillPath := filepath.Join(skillDir, SkillFileName)

	writeSkill := func(description string) {
		content := "---\nname: my-skill\ndescription: " + description + "\n---\nInstructions body.\n"
		require.NoError(t, os.WriteFile(skillPath, []byte(content), 0o600))
	}

	// Adding a new skill file.
	writeSkill("a test skill")
	waitNotified("onChange was not invoked after a new SKILL.md was added")
	requireSkillNamed(t, mgr, "my-skill", "a test skill")

	// Editing its description.
	time.Sleep(10 * time.Millisecond) // ensure a distinct mtime
	writeSkill("an updated test skill")
	waitNotified("onChange was not invoked after SKILL.md was edited")
	requireSkillNamed(t, mgr, "my-skill", "an updated test skill")

	// Removing it.
	require.NoError(t, os.Remove(skillPath))
	waitNotified("onChange was not invoked after SKILL.md was removed")
	for _, s := range mgr.ActiveSkills() {
		require.NotEqual(t, "my-skill", s.Name, "removed skill should no longer be active")
	}
}

// TestWatchForChanges_DetectsRootCreatedLater verifies that a discovery
// root that does not exist when the watcher starts (a project's first
// .braid/skills) is still picked up once it's created mid-session.
func TestWatchForChanges_DetectsRootCreatedLater(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "skills") // does not exist yet
	cfg := DiscoveryConfig{SkillsPaths: []string{root}}

	mgr := NewManager(nil, nil, nil)

	notified := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchForChanges(ctx, cfg, mgr, testPollInterval, func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	// Give the watcher time to take its first (empty, root missing) snapshot
	// before the root is created.
	time.Sleep(5 * testPollInterval)

	skillDir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: my-skill\ndescription: a test skill\n---\nInstructions body.\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(content), 0o600))

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("onChange was not invoked after the skills root was created and populated")
	}
	requireSkillNamed(t, mgr, "my-skill", "a test skill")
}

func requireSkillNamed(t *testing.T, mgr *Manager, name, description string) {
	t.Helper()
	for _, s := range mgr.ActiveSkills() {
		if s.Name == name {
			require.Equal(t, description, s.Description)
			return
		}
	}
	t.Fatalf("skill %q not found in ActiveSkills: %+v", name, mgr.ActiveSkills())
}
