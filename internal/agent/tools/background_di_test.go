package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

func TestBackgroundToolsUseInjectedManager(t *testing.T) {
	workingDir := t.TempDir()
	first, second := shell.NewBackgroundShellManager(), shell.NewBackgroundShellManager()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	firstResponse := runBashTool(t, newBashToolWithManager(workingDir, first), ctx, BashParams{Description: "A", Command: "echo A-output; sleep 5", RunInBackground: true})
	secondResponse := runBashTool(t, newBashToolWithManager(workingDir, second), ctx, BashParams{Description: "B", Command: "echo B-output; sleep 5", RunInBackground: true})
	require.False(t, firstResponse.IsError)
	require.False(t, secondResponse.IsError)
	var firstMeta, secondMeta BashResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(firstResponse.Metadata), &firstMeta))
	require.NoError(t, json.Unmarshal([]byte(secondResponse.Metadata), &secondMeta))
	require.Equal(t, firstMeta.ShellID, secondMeta.ShellID)

	require.Eventually(t, func() bool {
		return containsOutput(t, runBackgroundTool(t, NewJobOutputTool(first), ctx, JobOutputToolName, firstMeta.ShellID), "A-output")
	}, time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return containsOutput(t, runBackgroundTool(t, NewJobOutputTool(second), ctx, JobOutputToolName, secondMeta.ShellID), "B-output")
	}, time.Second, 10*time.Millisecond)

	killed := runBackgroundTool(t, NewJobKillTool(first), ctx, JobKillToolName, firstMeta.ShellID)
	require.False(t, killed.IsError)
	firstJob, found := first.Get(firstMeta.ShellID)
	require.False(t, found)
	require.Nil(t, firstJob)
	secondJob, found := second.Get(secondMeta.ShellID)
	require.True(t, found)
	require.False(t, secondJob.IsDone())
	require.False(t, runBackgroundTool(t, NewJobKillTool(second), ctx, JobKillToolName, secondMeta.ShellID).IsError)
}

func containsOutput(t *testing.T, response fantasy.ToolResponse, expected string) bool {
	t.Helper()
	return strings.Contains(response.Content, expected)
}

func runBackgroundTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name, id string) fantasy.ToolResponse {
	t.Helper()
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: name, Name: name, Input: `{"shell_id":"` + id + `"}`})
	require.NoError(t, err)
	return response
}

func TestBackgroundToolConstructorsRejectNilManager(t *testing.T) {
	require.PanicsWithValue(t, "background shell manager is required", func() {
		NewBashTool(&mockBashPermissionService{}, t.TempDir(), &config.Attribution{}, "test", nil)
	})
	require.PanicsWithValue(t, "background shell manager is required", func() { NewJobOutputTool(nil) })
	require.PanicsWithValue(t, "background shell manager is required", func() { NewJobKillTool(nil) })
}

func newBashToolWithManager(workingDir string, manager *shell.BackgroundShellManager) fantasy.AgentTool {
	permissions := &mockBashPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	return NewBashTool(permissions, workingDir, &config.Attribution{TrailerStyle: config.TrailerStyleNone}, "test-model", manager)
}
