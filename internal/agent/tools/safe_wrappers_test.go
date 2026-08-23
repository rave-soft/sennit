package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBashTool_WrapperCommandsStillRequirePermission pins the permission
// bypass that closed with the wrapper entries: safeCommands is matched by
// prefix, so listing a command that takes another command as its arguments
// made everything it wraps read-only too. `timeout 5 rm -rf ~` matched
// "timeout", skipped the prompt, and ran — bannedCommands does not carry
// rm, and there is no chaining metacharacter for containsCommandChaining
// to catch.
func TestBashTool_WrapperCommandsStillRequirePermission(t *testing.T) {
	for _, command := range []string{
		"timeout 5 rm -rf /tmp/nope",
		"nohup rm -rf /tmp/nope",
		"env rm -rf /tmp/nope",
		"nice rm -rf /tmp/nope",
		"time rm -rf /tmp/nope",
		"kill -9 -1",
		"killall -9 sshd",
	} {
		workingDir := t.TempDir()
		tool, perms := newBashToolWithRecordingPerms(workingDir, false)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

		resp := runBashTool(t, tool, ctx, BashParams{
			Description: "wrapper",
			Command:     command,
		})

		require.Equal(t, 1, perms.requestCount, "%q must ask before it runs", command)
		require.Contains(t, resp.Content, "User denied permission",
			"%q must not run once the request is denied", command)
	}
}

// TestBashTool_PlainReadOnlyCommandsStillSkipThePrompt is the other half:
// dropping the wrappers must not have made ordinary read-only commands
// prompt.
func TestBashTool_PlainReadOnlyCommandsStillSkipThePrompt(t *testing.T) {
	workingDir := t.TempDir()
	tool, perms := newBashToolWithRecordingPerms(workingDir, true)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	for _, command := range []string{"ls -la", "pwd", "whoami", "git status"} {
		perms.requestCount = 0
		resp := runBashTool(t, tool, ctx, BashParams{Description: "read only", Command: command})
		require.False(t, resp.IsError, "%q: %s", command, resp.Content)
		require.Zero(t, perms.requestCount, "%q reads only and must not prompt", command)
	}
}
