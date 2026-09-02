package dockermcp

import "context"

// SetVersionRunnerForTest stubs the "docker mcp version" probe for the
// duration of a test, so callers outside this package (e.g.
// internal/config's tests) can exercise EnableDockerMCP without shelling
// out. t must implement the minimal *testing.T surface used here to avoid
// this package importing "testing" into non-test builds.
func SetVersionRunnerForTest(t interface {
	Helper()
	Cleanup(func())
}, runner func(context.Context) error,
) {
	t.Helper()
	orig := versionRunner
	versionRunner = runner
	t.Cleanup(func() {
		versionRunner = orig
	})
}
