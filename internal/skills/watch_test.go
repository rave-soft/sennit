package skills

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = withWatchStartedHook(ctx, func() { close(started) })
	go WatchForChanges(ctx, func() DiscoveryConfig { return cfg }, mgr, testPollInterval, func() {
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

	// Wait for the watcher's first snapshot to actually complete before
	// any file exists, so the write below is seen as a genuine diff
	// rather than racing to land inside that very first scan (which
	// would fold it into the baseline and make it undetectable).
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not take its first snapshot in time")
	}

	skillDir := filepath.Join(root, "my-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	skillPath := filepath.Join(skillDir, SkillFileName)

	// writeSkill writes skillPath the way the real Write/Edit tool does
	// (see fsext.AtomicCreateFile): to a temp file, then renamed into
	// place. A plain os.WriteFile truncates the target before writing its
	// new content, so a poll landing in that window sees a momentarily
	// empty file — a torn read the watcher would never see from an actual
	// external edit, and reproducible often enough under -race plus CPU
	// load to be the real source of this test's CI flakiness.
	writeSkill := func(description string) {
		content := "---\nname: my-skill\ndescription: " + description + "\n---\nInstructions body.\n"
		tmp, err := os.CreateTemp(skillDir, "SKILL.*.tmp")
		require.NoError(t, err)
		_, err = tmp.Write([]byte(content))
		require.NoError(t, err)
		require.NoError(t, tmp.Close())
		require.NoError(t, os.Rename(tmp.Name(), skillPath))
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
// .sennit/skills) is still picked up once it's created mid-session.
func TestWatchForChanges_DetectsRootCreatedLater(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	root := filepath.Join(parent, "skills") // does not exist yet
	cfg := DiscoveryConfig{SkillsPaths: []string{root}}

	mgr := NewManager(nil, nil, nil)

	notified := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchForChanges(ctx, func() DiscoveryConfig { return cfg }, mgr, testPollInterval, func() {
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

// A workspace that inherited skills from a parent keeps them when its own
// discovery re-runs. The inherited set is on no path this workspace scans,
// so a re-discovery that ignored it would silently drop a thread's skills
// the first time any local SKILL.md changed.
func TestWatchForChanges_KeepsInheritedSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	inherited := &Skill{
		Name:         "parent-skill",
		Description:  "handed down",
		Instructions: "Parent instructions.",
	}
	mgr := NewManager(nil, nil, nil, WithInheritedSkills([]*Skill{inherited}))

	notified := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchForChanges(ctx, func() DiscoveryConfig {
		return DiscoveryConfig{SkillsPaths: []string{root}, InheritedSkills: mgr.InheritedSkills()}
	}, mgr, testPollInterval, func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	})

	time.Sleep(5 * testPollInterval)

	skillDir := filepath.Join(root, "local-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, SkillFileName),
		[]byte("---\nname: local-skill\ndescription: the workspace's own\n---\nBody.\n"),
		0o600,
	))

	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("onChange was not invoked after a local SKILL.md was added")
	}

	requireSkillNamed(t, mgr, "local-skill", "the workspace's own")
	requireSkillNamed(t, mgr, "parent-skill", "handed down")
}

// TestScanSkillFiles_ConcurrentWrites spreads enough SKILL.md files across
// enough subdirectories that fastwalk hands them to multiple workers at
// once, so a run under `go test -race` catches an unguarded concurrent
// write into the snapshot map.
func TestScanSkillFiles_ConcurrentWrites(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	const numSkills = 50
	want := make([]string, 0, numSkills)
	for i := 0; i < numSkills; i++ {
		skillDir := filepath.Join(root, "skill-"+strconv.Itoa(i))
		require.NoError(t, os.MkdirAll(skillDir, 0o755))
		path := filepath.Join(skillDir, SkillFileName)
		require.NoError(t, os.WriteFile(path, []byte("SKILL.md contents"), 0o600))
		want = append(want, path)
	}

	snapshot := scanSkillFiles([]string{root})

	require.Len(t, snapshot, numSkills)
	for _, path := range want {
		_, ok := snapshot[path]
		require.True(t, ok, "missing snapshot entry for %s", path)
	}
}
