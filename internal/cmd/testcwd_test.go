package cmd

import (
	"testing"

	"github.com/rave-soft/sennit/internal/testenv"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// setCwdFlag sets cmd's --cwd flag to dir and registers a cleanup that
// restores the test process's real working directory. ResolveCwd
// (internal/cmd/root.go) os.Chdir's into --cwd for production correctness
// and nothing there restores it afterward -- intentional, but it leaks
// across tests: whichever test runs a command with --cwd set to a
// t.TempDir() leaves the process cwd inside that directory, which Windows
// then refuses to RemoveAll. Routing every "cwd" flag set through this
// helper, right where dir is already known to exist, means the restore
// cleanup is always registered after dir's own t.TempDir() cleanup and so
// (by t.Cleanup's LIFO order) runs before it.
func setCwdFlag(t *testing.T, cmd *cobra.Command, dir string) {
	t.Helper()
	testenv.RestoreCwd(t)
	require.NoError(t, cmd.Flags().Set("cwd", dir))
}
