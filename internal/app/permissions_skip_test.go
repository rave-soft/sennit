package app

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
