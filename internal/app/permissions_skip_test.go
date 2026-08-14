package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// recordingSkipPropagator stands in for *thread.Manager, which this
// package cannot reference (internal/thread imports internal/app).
type recordingSkipPropagator struct {
	calls []bool
}

func (p *recordingSkipPropagator) SetPermissionsSkip(skip bool) {
	p.calls = append(p.calls, skip)
}

// TestSetPermissionsSkipPropagatesToThreads pins the funnel every yolo
// toggle goes through. Threads run in isolated apps with permission
// services of their own, so setting only this app's flag - which is what
// callers used to do by reaching for Permissions.SetSkipRequests directly
// - leaves running threads behind.
func TestSetPermissionsSkipPropagatesToThreads(t *testing.T) {
	t.Parallel()

	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)

	propagator := &recordingSkipPropagator{}
	app.SetThreadManager(propagator)

	app.SetPermissionsSkip(true)
	require.True(t, app.Permissions().SkipRequests(), "the workspace's own flag must be set")
	require.Equal(t, []bool{true}, propagator.calls, "and forwarded to live threads")

	app.SetPermissionsSkip(false)
	require.False(t, app.Permissions().SkipRequests())
	require.Equal(t, []bool{true, false}, propagator.calls,
		"turning bypass off must propagate too")
}

// TestSetPermissionsSkipWithoutThreadManager covers the workspaces that
// own no thread manager at all - non-git ones, and thread workspaces
// themselves, where nesting is not supported.
func TestSetPermissionsSkipWithoutThreadManager(t *testing.T) {
	t.Parallel()

	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)

	require.NotPanics(t, func() { app.SetPermissionsSkip(true) })
	require.True(t, app.Permissions().SkipRequests())
}

// TestPermissionsSkipFuncTracksLiveState is the regression test for what
// threads actually got wrong: the accessor the spawners hand a new thread
// must report the state the user is in *now*, not the --yolo flag the
// process started with. A version reading Store().Overrides() cannot pass
// this — the override is written once, at bootstrap.
func TestPermissionsSkipFuncTracksLiveState(t *testing.T) {
	t.Parallel()

	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)

	skip := app.PermissionsSkipFunc()
	require.False(t, skip(), "precondition: started without bypass")

	app.SetPermissionsSkip(true)
	require.True(t, skip(),
		"a thread spawned after the user turned bypass on must inherit it")

	app.SetPermissionsSkip(false)
	require.False(t, skip(),
		"and a thread spawned after they turned it back off must not")
}
