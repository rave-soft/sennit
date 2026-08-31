//go:build linux

package tools

import (
	"fmt"
	"os/exec"

	"github.com/rave-soft/sennit/internal/permission"
)

// confinedBashCommand runs confined-workspace commands under bubblewrap.
// The mount namespace exposes the host read-only while bind-mounting the
// workspace read-write at its original path, so shell expansion and symlink
// resolution cannot mutate any host path outside the workspace.
func confinedBashCommand(permissions permission.Requester, workingDir, command string) (string, error) {
	return confinedBashCommandWithLookup(permissions, workingDir, command, exec.LookPath)
}

// confinedBashCommandWithLookup isolates lookup so its security-critical
// command construction can be tested without depending on bubblewrap being
// installed on the test machine.
func confinedBashCommandWithLookup(permissions permission.Requester, workingDir, command string, lookup func(string) (string, error)) (string, error) {
	if permissions == nil || permissions.ConfinedDir() == "" {
		return command, nil
	}
	bwrap, err := lookup("bwrap")
	if err != nil {
		return "", fmt.Errorf("refusing to run: confined workspace requires bubblewrap runtime isolation, but it is unavailable")
	}
	return fmt.Sprintf("%q --die-with-parent --unshare-user --unshare-pid --unshare-net --ro-bind / / --bind %q %q --chdir %q --proc /proc --dev /dev --new-session -- sh -c %q", bwrap, workingDir, workingDir, workingDir, command), nil
}
