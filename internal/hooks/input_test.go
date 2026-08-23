package hooks

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildEnv_StripsHerdrVars ensures a hook command never inherits herdr's
// pane-ownership vars. Without stripping them, a hook that shells out to a
// nested sennit would attach to the parent pane and a buffered "working"
// report could be delivered after the parent already released it.
func TestBuildEnv_StripsHerdrVars(t *testing.T) {
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv("HERDR_PANE_ID", "pane-1")

	env := BuildEnv("PreToolUse", "bash", "session-1", "/cwd", "/project", `{}`)

	for _, e := range env {
		if strings.HasPrefix(e, "HERDR_") {
			t.Fatalf("BuildEnv leaked herdr var into hook environment: %q", e)
		}
	}
	require.NotEmpty(t, env)
}
