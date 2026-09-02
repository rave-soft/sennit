package threadspawn

import (
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// TestAttach_ForwardersStopAfterShutdown is the regression test for
// forwardSkillsToThreads/forwardAgentsToThreads outliving the App they
// forward for. Both used to run on the command context passed into Attach
// (ctx here), which nothing cancels when a.Shutdown() runs -
// parent.Skills.SubscribeEvents(ctx) does not close the way
// parent.Events(ctx) does when the broker shuts down - so the goroutines,
// and the *app.App reference they hold, lived until process exit. They
// must now stop once Shutdown runs, the same way the config/skills
// watchers already do (see app.startExternalChangeWatchers).
func TestAttach_ForwardersStopAfterShutdown(t *testing.T) {
	repo := initRepo(t)
	a := newAttachTestApp(t, repo)

	// Captured after the App (and its own background watchers) exist, so
	// the diff below reports only what Attach itself started.
	ignoreBaseline := goleak.IgnoreCurrent()

	spawner := NewLocalSpawner(
		func() map[string]config.Agent { return a.Config().UserAgents() },
		func() []*skills.Skill { return skills.Inheritable(a.Skills.AllSkills()) },
		a.PermissionsSkipFunc(),
		func() config.SelectedModel { return a.Config().Model },
	)

	Attach(t.Context(), a, repo, spawner)
	require.NotNil(t, a.ThreadManager(), "precondition: attach ran the *LocalSpawner branch that starts the forwarders")

	a.Shutdown()

	// Shutdown's own cancel/join is synchronous, but goleak needs a moment
	// to observe the forwarder goroutines actually exit the scheduler.
	deadline := time.Now().Add(2 * time.Second)
	var leakErr error
	for time.Now().Before(deadline) {
		leakErr = goleak.Find(ignoreBaseline)
		if leakErr == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("forwardSkillsToThreads/forwardAgentsToThreads must stop after Shutdown, not outlive their App: %v", leakErr)
}
