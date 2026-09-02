package agent

import (
	"context"
	"encoding/json"
	"net/http"
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
	viewTool := tools.NewReadTool(nil, perms, newFileTracking(fakeFileTracker{}), nil, tmpDir)

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

// TestAgenticFetchTool_SharesClientAcrossCalls guards the fix for
// agenticFetchTool cloning a fresh *http.Transport (and its idle-connection
// pool) on every call: two calls with client == nil must resolve to the
// same *http.Client, and a caller-supplied client must still be used as-is
// without ever populating the shared one.
func TestAgenticFetchTool_SharesClientAcrossCalls(t *testing.T) {
	t.Parallel()

	d := &delegationFinalizer{agentDeps: &agentDeps{}}

	_, err := d.agenticFetchTool(t.Context(), nil)
	require.NoError(t, err)
	first := d.fetchClient
	require.NotNil(t, first, "agenticFetchTool(nil) must build a shared client")

	_, err = d.agenticFetchTool(t.Context(), nil)
	require.NoError(t, err)
	require.Same(t, first, d.fetchClient, "a second nil-client call must reuse the same *http.Client")

	other := &delegationFinalizer{agentDeps: &agentDeps{}}
	custom := &http.Client{}
	_, err = other.agenticFetchTool(t.Context(), custom)
	require.NoError(t, err)
	require.Nil(t, other.fetchClient, "a caller-supplied client must be used as-is, not replaced by the shared one")
}

// TestBuildAgenticFetchAgent_IsSubAgentAndToolSet guards two properties of
// the agentic-fetch delegate agenticFetchFactory builds by hand instead of
// through buildAgent (see that method's doc comment):
//
//  1. IsSubAgent is set. Before this was added, preparePrompt (compat.go)
//     treated the delegate as the top-level agent and injected the parent
//     todo-reminder system message into it - a reminder pointing at a
//     "todos" tool this scratch-dir analysis agent was never given.
//  2. Its tool set is exactly the five tmpDir-scoped tools it hand-builds
//     (web_fetch, web_search, glob, the available search tool, read) - no
//     more, no less - proving the hand-written list still matches intent
//     even though it cannot be sourced from the shared toolSpecs registry
//     (see the doc comment on why: every toolSpecs row is rooted at the
//     workspace's real working directory, not a disposable scratch one).
func TestBuildAgenticFetchAgent_IsSubAgentAndToolSet(t *testing.T) {
	coord := newAgentToolTestCoordinator(t, nil)

	agent, err := coord.delegation.buildAgenticFetchAgent(t.Context(), nil, t.TempDir())
	require.NoError(t, err)

	sa, ok := agent.(*sessionAgent)
	require.True(t, ok, "buildAgenticFetchAgent must return a *sessionAgent")
	require.True(t, sa.isSubAgent, "the agentic-fetch delegate must be marked IsSubAgent so it does not get the parent todo-reminder")

	require.ElementsMatch(t,
		[]string{tools.WebFetchToolName, tools.WebSearchToolName, tools.GlobToolName, searchToolName(), tools.ReadToolName},
		toolNames(t, sa.tools.Copy()),
		"the agentic-fetch delegate must get exactly its hand-picked, tmpDir-scoped tool set",
	)
}
