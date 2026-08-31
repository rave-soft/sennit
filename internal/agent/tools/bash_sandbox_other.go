//go:build !linux

package tools

import (
	"fmt"

	"github.com/rave-soft/sennit/internal/permission"
)

// Confined workspaces fail closed where the supported Linux namespace sandbox
// is unavailable. Unconfined workspaces continue to use the ordinary shell.
func confinedBashCommand(permissions permission.Requester, workingDir, command string) (string, error) {
	return confinedBashCommandWithLookup(permissions, workingDir, command, nil)
}

func confinedBashCommandWithLookup(permissions permission.Requester, _, command string, _ func(string) (string, error)) (string, error) {
	if permissions == nil || permissions.ConfinedDir() == "" {
		return command, nil
	}
	return "", fmt.Errorf("refusing to run: runtime isolation for confined workspaces is unsupported on this operating system")
}
