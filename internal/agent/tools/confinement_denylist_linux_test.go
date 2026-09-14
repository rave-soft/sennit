//go:build linux

package tools

import (
	"testing"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBashTool_DenyListSurvivesSandboxWrapping is the regression test for a
// deny list that was matched against the sandboxed command rather than the
// one the model asked for. A confined workspace rewrites `sudo apt-get
// install nmap` into `bwrap … -- sh -c "sudo apt-get install nmap"`, whose
// only CallExpr is bwrap itself: shell.BlockedBy deliberately does not
// descend into a command's words, so every deny-listed command inside the
// wrapper read as not blocked. The floor was therefore absent in exactly
// the workspaces that run unattended - a thread in its own worktree,
// which inherits the main agent's yolo.
//
// newConfinedTestBashTool's identity sandboxCommand is what let this pass
// unnoticed, so this test wires the real wrapper instead.
func TestBashTool_DenyListSurvivesSandboxWrapping(t *testing.T) {
	workdir := t.TempDir()
	perms := &recordingConfinedPermissions{
		confinedTestPermissions: &confinedTestPermissions{dir: workdir},
	}
	sandbox := func(p permission.Requester, workingDir, command string) (string, error) {
		return confinedBashCommandWithLookup(p, workingDir, command, func(string) (string, error) {
			return "/usr/bin/bwrap", nil
		})
	}
	tool := newBashTool(perms, workdir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", shell.NewBackgroundShellManager(), sandbox)

	resp, err := tool.Run(confinedTestCtx(t), fantasy.ToolCall{
		ID:    "call-1",
		Name:  BashToolName,
		Input: mustJSONInput(t, BashParams{Command: "sudo apt-get install nmap"}),
	})
	require.NoError(t, err)

	require.Len(t, perms.requests, 1, "a deny-listed command asks exactly once")
	assert.True(t, perms.requests[0].RequireExplicit, "the deny list must reach the person even inside a confined workspace")
	assert.Contains(t, perms.requests[0].Description, "deny-listed")
	assert.Contains(t, resp.Content, "run it manually")
	assert.True(t, resp.StopTurn)
}
