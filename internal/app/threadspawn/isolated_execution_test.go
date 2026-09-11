package threadspawn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func TestIsolatedExecutionUsesSelectedAgentAndModel(t *testing.T) {
	requests := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"selected\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"selected result\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"selected\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	global := t.TempDir()
	t.Setenv("SENNIT_GLOBAL_CONFIG", global)
	t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(global, "data"))
	configuration := fmt.Sprintf(`{"options":{"disable_default_providers":true},"providers":{"mock":{"id":"mock","name":"Mock","type":"openai","base_url":%q,"api_key":"test","models":[{"id":"selected","name":"Selected","context_window":8192,"default_max_tokens":1024},{"id":"other","name":"Other","context_window":8192,"default_max_tokens":1024}]}},"model":{"provider":"mock","model":"other"}}`, server.URL+"/v1")
	require.NoError(t, os.WriteFile(filepath.Join(global, "sennit.json"), []byte(configuration), 0o600))
	repo := initRepo(t)
	path := filepath.Join(t.TempDir(), "isolated")
	definition := config.Agent{ID: "selected-reviewer", Name: "Selected reviewer", Prompt: "SELECTED_SPECIALIZATION", AllowedTools: []string{"read"}}
	spec := agent.DelegationExecution{
		AgentID: definition.ID, ParentSessionID: "parent", SessionID: "fixed-child", SessionTitle: "Selected title",
		Goal: "inspect repository", Depth: 2, Definition: definition,
		Model: config.SelectedModel{Provider: "mock", Model: "selected", MaxTokens: 321}, HistoryFrozen: true,
	}
	encoded, err := json.Marshal(spec)
	require.NoError(t, err)
	spawner := NewLocalSpawner(nil, nil, nil, nil)
	runtime, err := isolatedTaskRuntime(repo, spawner)(t.Context(), thread.TaskCreateArgs{
		Goal: spec.Goal, AgentID: definition.ID, SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID,
		Isolation: "worktree", Execution: string(encoded), WorktreePath: path, Branch: "thread/selected", BaseBranch: "main",
	})
	if runtime.Handle != nil {
		t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), runtime.Handle.ID())) })
	}
	require.NoError(t, err)
	require.Equal(t, 2, runtime.Depth)
	require.Equal(t, path, runtime.Handle.Workspace().Permissions().ConfinedDir())
	_, err = runtime.Handle.Workspace().Sessions().CreateSubAgentSession(t.Context(), spec.SessionID, spec.ParentSessionID, spec.SessionTitle, spec.AgentID)
	require.NoError(t, err)
	run, cleanup, err := runtime.Factory(t.Context(), spec.SessionID)
	require.NoError(t, err)
	if cleanup != nil {
		defer cleanup()
	}
	result, err := run(t.Context())
	require.NoError(t, err)
	require.Equal(t, "selected result", result.Text)
	var executionRequest string
	for len(requests) != 0 {
		body := <-requests
		if strings.Contains(body, "SELECTED_SPECIALIZATION") {
			executionRequest = body
		}
	}
	require.NotEmpty(t, executionRequest)
	require.Contains(t, executionRequest, `"model":"selected"`)
	require.Contains(t, executionRequest, `"name":"read"`)
	require.NotContains(t, executionRequest, `"name":"write"`)
	require.NotContains(t, executionRequest, `"name":"bash"`)
	require.Contains(t, executionRequest, "321")
	require.NoError(t, spawner.Release(t.Context(), runtime.Handle.ID()))
	resumed, err := isolatedTaskRuntime(repo, spawner)(t.Context(), thread.TaskCreateArgs{
		Goal: "continue selected work", SessionID: spec.SessionID, ParentSessionID: spec.ParentSessionID,
		Isolation: "worktree", Execution: string(encoded), WorktreePath: path, Branch: "thread/selected", BaseBranch: "main", Resume: true,
	})
	if resumed.Handle != nil {
		t.Cleanup(func() { require.NoError(t, spawner.Release(context.Background(), resumed.Handle.ID())) })
	}
	require.NoError(t, err)
	require.Equal(t, 2, resumed.Depth)
	run, cleanup, err = resumed.Factory(t.Context(), spec.SessionID)
	require.NoError(t, err)
	if cleanup != nil {
		defer cleanup()
	}
	result, err = run(t.Context())
	require.NoError(t, err)
	require.Equal(t, "selected result", result.Text)
	var resumedRequest string
	for len(requests) != 0 {
		body := <-requests
		if strings.Contains(body, "continue selected work") {
			resumedRequest = body
		}
	}
	require.NotEmpty(t, resumedRequest)
	require.Contains(t, resumedRequest, "SELECTED_SPECIALIZATION")
	require.Contains(t, resumedRequest, `"model":"selected"`)
	require.Contains(t, resumedRequest, "inspect repository")
	require.NotContains(t, resumedRequest, `"name":"write"`)
	local := resumed.Handle.(*localHandle)
	parentSpawner := NewParentAppSpawner(resumed.Handle.Workspace())
	shared, err := sharedTaskRuntime(local.app.Coordinator, parentSpawner)(t.Context(), thread.TaskCreateArgs{
		Goal: "shared specialized continuation", SessionID: spec.SessionID, Execution: string(encoded), Resume: true,
	})
	require.NoError(t, err)
	run, _, err = shared.Factory(t.Context(), spec.SessionID)
	require.NoError(t, err)
	_, err = run(t.Context())
	require.NoError(t, err)
	var sharedRequest string
	for len(requests) != 0 {
		body := <-requests
		if strings.Contains(body, "shared specialized continuation") {
			sharedRequest = body
		}
	}
	require.Contains(t, sharedRequest, "SELECTED_SPECIALIZATION")
	require.Contains(t, sharedRequest, `"model":"selected"`)
	require.NotContains(t, sharedRequest, `"name":"write"`)
	require.NotContains(t, sharedRequest, `"name":"bash"`)
}
