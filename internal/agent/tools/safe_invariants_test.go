package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// commandExecutors are commands that take another command as their
// arguments and run it. None may ever appear in safeCommands: membership
// there is decided by prefix match, so listing an executor makes every
// command it can be handed read-only too. This is the list the `timeout 5
// rm -rf ~` bypass was closed by removing entries from.
//
// It is a denylist, and a denylist can only catch executors somebody
// thought of. It earns its place anyway: the failure it guards is silent
// and total — a new entry here does not degrade the permission prompt, it
// removes it — and the reviewer of a future addition to safeCommands is
// more likely to read a failing test than this file's doc comment.
var commandExecutors = []string{
	"aprun", "bash", "chroot", "command", "dash", "doas", "env", "eval",
	"exec", "fish", "ionice", "ksh", "nice", "node", "nohup", "nsenter",
	"parallel", "perl", "pkexec", "proot", "python", "python3", "ruby",
	"runuser", "script", "setarch", "setsid", "sh", "srun", "stdbuf",
	"su", "sudo", "taskset", "tcsh", "time", "timeout", "unbuffer",
	"watch", "xargs", "zsh",
}

// TestSafeCommands_ContainNoExecutors is the standing form of the wrapper
// bypass: whatever else changes about the list, no entry of it may be a
// command that runs its own arguments.
func TestSafeCommands_ContainNoExecutors(t *testing.T) {
	t.Parallel()

	for _, entry := range safeCommands {
		first, _, _ := strings.Cut(entry, " ")
		require.NotContains(t, commandExecutors, first,
			"safeCommands entry %q runs its arguments, so listing it makes every command it wraps skip the permission prompt", entry)
	}
}

// TestArgumentGatedSafeCommands_AreListedAsSafe keeps the gate table from
// drifting away from the list it gates. A key that is no longer a
// safeCommands entry gates nothing and reads as protection that is not
// there.
func TestArgumentGatedSafeCommands_AreListedAsSafe(t *testing.T) {
	t.Parallel()

	for entry := range argumentGatedSafeCommands {
		require.Contains(t, safeCommands, entry,
			"argumentGatedSafeCommands gates %q, which safeCommands no longer carries", entry)
	}
}

// TestBashTool_MutatingGitInvocationsRequirePermission pins the second
// bypass of this shape found in the list. All five ran with no prompt at
// all before the argument gate: safeCommands carried the bare read-only
// forms `git branch`, `git tag` and `git remote`, prefix matching accepted
// anything that followed, and bannedCommands carries no git.
func TestBashTool_MutatingGitInvocationsRequirePermission(t *testing.T) {
	for _, command := range []string{
		"git branch -D nope",
		"git branch --delete nope",
		"git branch newbranch",
		// A read-only flag must not escort a destructive one past the
		// gate: git honours the -D here.
		"git branch -v -D nope",
		"git tag -d nope",
		"git tag v99",
		"git remote remove origin",
		"git remote set-url origin https://example.invalid/x.git",
	} {
		workingDir := t.TempDir()
		tool, perms := newBashToolWithRecordingPerms(workingDir, false)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

		resp := runBashTool(t, tool, ctx, BashParams{
			Description: "mutating git",
			Command:     command,
		})

		require.Equal(t, 1, perms.requestCount, "%q must ask before it runs", command)
		require.Contains(t, resp.Content, "User denied permission",
			"%q must not run once the request is denied", command)
	}
}

// TestBashTool_ReadOnlyGitInvocationsSkipThePrompt is the other half of
// the gate: it must not cost the listing forms their prompt-free path,
// which is the whole reason these three entries are in safeCommands.
func TestBashTool_ReadOnlyGitInvocationsSkipThePrompt(t *testing.T) {
	for _, command := range []string{
		"git branch",
		"git branch -a",
		"git branch --list",
		"git branch -v -a",
		"git tag",
		"git tag -l",
		"git remote",
		"git remote -v",
	} {
		workingDir := t.TempDir()
		tool, perms := newBashToolWithRecordingPerms(workingDir, false)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

		runBashTool(t, tool, ctx, BashParams{
			Description: "read-only git",
			Command:     command,
		})

		require.Equal(t, 0, perms.requestCount, "%q lists and must not ask", command)
	}
}

// TestIsReadOnlyInvocation_UngatedEntriesAreAlwaysReadOnly states the
// table's default directly: an entry with no gate is safe in every
// argument form, which is what puts the burden on adding one.
func TestIsReadOnlyInvocation_UngatedEntriesAreAlwaysReadOnly(t *testing.T) {
	t.Parallel()

	for _, entry := range safeCommands {
		if _, gated := argumentGatedSafeCommands[entry]; gated {
			continue
		}
		require.True(t, isReadOnlyInvocation(entry, entry+" whatever it likes"))
	}
	require.True(t, slices.Contains(safeCommands, "git log"))
}
