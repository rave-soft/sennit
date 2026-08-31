//go:build linux

package tools

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfinedBashCommandBuildsNamespaceBoundary(t *testing.T) {
	workdir, _, permissions := writeOutsideAttempt(t)
	command, err := confinedBashCommandWithLookup(permissions, workdir, `OUT=$(pwd)/../outside; echo leaked > "$OUT"`, func(name string) (string, error) {
		require.Equal(t, "bwrap", name)
		return "/test/bin/bwrap", nil
	})
	require.NoError(t, err)
	require.Contains(t, command, `"/test/bin/bwrap" --die-with-parent --unshare-user --unshare-pid --unshare-net`)
	require.Contains(t, command, `--ro-bind / / --bind `)
	require.Contains(t, command, fmt.Sprintf(`%q %q`, workdir, workdir))
	require.Contains(t, command, `--chdir `+fmt.Sprintf("%q", workdir))
	require.Contains(t, command, `--proc /proc --dev /dev --new-session -- sh -c `)
}

func TestConfinedBashCommandFailsClosedWithoutBubblewrap(t *testing.T) {
	workdir, _, permissions := writeOutsideAttempt(t)
	_, err := confinedBashCommandWithLookup(permissions, workdir, "echo safe", func(string) (string, error) {
		return "", os.ErrNotExist
	})
	require.ErrorContains(t, err, "requires bubblewrap runtime isolation")
}
