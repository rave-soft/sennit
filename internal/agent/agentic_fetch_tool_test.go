package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/stretchr/testify/require"
)

// fakeFileTracker is a no-op filetracker.Service for tests that only need
// the interface satisfied, not its bookkeeping.
type fakeFileTracker struct{}

func (fakeFileTracker) RecordRead(context.Context, string, string) {}

func (fakeFileTracker) LastReadTime(context.Context, string, string) time.Time {
	return time.Time{}
}

func (fakeFileTracker) ListReadFiles(context.Context, string) ([]string, error) {
	return nil, nil
}

func (fakeFileTracker) RecordPartialRead(context.Context, string, string, int, int) {}

func (fakeFileTracker) RecordEdit(context.Context, string, string, int, int, int) {}

func (fakeFileTracker) ReadCoverage(context.Context, string, string) filetracker.Coverage {
	return filetracker.FullCoverage
}

// TestAgenticFetchSubAgentView_OutsideWorkdirRequiresPermission guards the
// fix for the agentic_fetch permission hole: the sub-agent's session used
// to be auto-approved (c.permissions.AutoApproveSession), which let its
// NewReadTool silently read any file on disk outside the fetch tmpDir. The
// sub-agent's session must now go through the normal permission flow, same
// as any other agent-as-tool sub-agent, so a view outside the sandboxed
// tmpDir surfaces a real request instead of being nodded through.
func TestAgenticFetchSubAgentView_OutsideWorkdirRequiresPermission(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("do not read me"), 0o644))

	// Built the same way agenticFetchTool assembles fetchTools: a real
	// permission.Service, with no AutoApproveSession call for the child
	// session.
	perms := permission.NewPermissionService(tmpDir, false, nil)
	viewTool := tools.NewReadTool(nil, perms, fakeFileTracker{}, nil, tmpDir)

	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "fetch-child-session")

	sub := perms.Subscribe(ctx)

	input, err := json.Marshal(tools.ReadParams{FilePath: outsideFile})
	require.NoError(t, err)
	call := fantasy.ToolCall{ID: "call-1", Name: tools.ReadToolName, Input: string(input)}

	done := make(chan struct{})
	var deniedResp bool
	go func() {
		defer close(done)
		resp, runErr := viewTool.Run(ctx, call)
		require.NoError(t, runErr)
		deniedResp = resp.IsError
	}()

	select {
	case evt := <-sub:
		require.Equal(t, "fetch-child-session", evt.Payload.SessionID)
		perms.Deny(evt.Payload)
	case <-time.After(5 * time.Second):
		t.Fatal("expected a permission request for the out-of-workdir view, got none " +
			"(the child session may be auto-approved again)")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("view tool did not return after permission was denied")
	}
	require.True(t, deniedResp, "denied out-of-workdir view should surface as an error response")
}
