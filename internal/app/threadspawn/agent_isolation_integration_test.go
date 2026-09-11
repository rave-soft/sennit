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
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

func isolationStream(w http.ResponseWriter, delta any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
	fmt.Fprintf(w, "data: %s\n\n", payload)
	payload, _ = json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
}

func TestAgentIsolationAdmissionMatrix(t *testing.T) {
	for _, mode := range []string{"anonymous", "reviewer", "writer"} {
		named := mode != "anonymous"
		writer := mode == "writer"
		for _, isolation := range []string{"", "worktree"} {
			t.Run(fmt.Sprintf("%s/isolation=%s", mode, isolation), func(t *testing.T) {
				repo := initRepo(t)
				writeStep := 0
				var mu sync.Mutex
				var requests []string
				launched := false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), 400)
						return
					}
					mu.Lock()
					requests = append(requests, string(body))
					launch := !launched && strings.Contains(string(body), "LAUNCH_MATRIX_PARENT") && strings.Contains(string(body), `"name":"agent"`)
					if launch {
						launched = true
					}
					writing := writer && strings.Contains(string(body), `"model":"selected"`) && !strings.Contains(string(body), "MATRIX_FOLLOWUP") && writeStep < 2
					step := writeStep
					if writing {
						writeStep++
					}
					mu.Unlock()
					if writing {
						target := "matrix-written.txt"
						if step == 1 {
							target = filepath.Join(repo, "matrix-parent-denied.txt")
						}
						args, _ := json.Marshal(map[string]string{"file_path": target, "content": "matrix write"})
						isolationStream(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": fmt.Sprintf("write-%d", step), "type": "function", "function": map[string]any{"name": "write", "arguments": string(args)}}}}, "tool_calls")
						return
					}
					if launch {
						params := map[string]string{"prompt": "MATRIX_CHILD_GOAL", "description": "Matrix child title", "isolation": isolation}
						if named {
							params["subagent_type"] = "matrix-reviewer"
						}
						args, _ := json.Marshal(params)
						isolationStream(w, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "matrix-call", "type": "function", "function": map[string]any{"name": "agent", "arguments": string(args)}}}}, "tool_calls")
						return
					}
					isolationStream(w, map[string]any{"role": "assistant", "content": "MATRIX_RESULT"}, "stop")
				}))
				defer server.Close()
				global := t.TempDir()
				t.Setenv("SENNIT_GLOBAL_CONFIG", global)
				t.Setenv("SENNIT_GLOBAL_DATA", filepath.Join(global, "data"))
				configuration := fmt.Sprintf(`{"permissions":{"allowed_tools":["write"]},"options":{"disable_default_providers":true},"providers":{"mock":{"id":"mock","name":"Mock","type":"openai","base_url":%q,"api_key":"test","models":[{"id":"selected","name":"Selected","context_window":32768,"default_max_tokens":1024},{"id":"parent","name":"Parent","context_window":32768,"default_max_tokens":1024}]}},"model":{"provider":"mock","model":"parent"}}`, server.URL+"/v1")
				require.NoError(t, os.WriteFile(filepath.Join(global, "sennit.json"), []byte(configuration), 0o600))
				allowedTools := []string{"read"}
				if writer {
					allowedTools = append(allowedTools, "write")
				}
				boot, err := app.Bootstrap(t.Context(), repo, app.BootstrapOptions{YOLO: true, InheritedAgents: map[string]config.Agent{"matrix-reviewer": {ID: "matrix-reviewer", Name: "Matrix reviewer", Prompt: "MATRIX_SPECIALIZATION", Model: "mock/selected", AllowedTools: allowedTools}}})
				require.NoError(t, err)
				defer boot.App.Shutdown()
				spawner := NewLocalSpawner(nil, nil, nil, nil)
				Attach(t.Context(), boot.App, repo, spawner)
				require.NoError(t, boot.App.InitCoderAgentNonInteractive(t.Context()))
				parent, err := boot.App.Sessions().Create(t.Context(), "Matrix parent")
				require.NoError(t, err)
				prior, err := boot.App.Sessions().CreateSubAgentSession(t.Context(), "matrix-prior", parent.ID, "Prior", "matrix-reviewer")
				require.NoError(t, err)
				_, err = boot.App.Messages().Create(t.Context(), prior.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "MATRIX_PRIOR_HISTORY"}}})
				require.NoError(t, err)
				_, err = boot.App.Coordinator().Run(t.Context(), parent.ID, "LAUNCH_MATRIX_PARENT")
				require.NoError(t, err)
				var task thread.Thread
				require.Eventually(t, func() bool {
					tasks, err := boot.App.TaskManager().List(t.Context())
					if err != nil || len(tasks) != 1 {
						return false
					}
					task = tasks[0]
					return task.Status == thread.StatusCompleted || task.Status == thread.StatusFailed
				}, 20*time.Second, 10*time.Millisecond)
				require.Equal(t, thread.StatusCompleted, task.Status, task.Error)
				require.Equal(t, parent.ID, task.ParentSessionID)
				require.Equal(t, 1, task.CompletionDepth)
				// The snapshot is read back through Get, not from the
				// listing above: listings deliberately omit the execution
				// column, which is only needed on resume.
				stored, err := boot.App.TaskManager().Get(t.Context(), task.ID)
				require.NoError(t, err)
				var spec agent.DelegationExecution
				require.NoError(t, json.Unmarshal([]byte(stored.Execution), &spec))
				require.Equal(t, task.SessionID, spec.SessionID)
				require.Equal(t, "Matrix child title", spec.SessionTitle)
				require.Equal(t, 1, spec.Depth)
				messages, err := boot.App.Messages().List(t.Context(), parent.ID)
				require.NoError(t, err)
				expectedID := ""
				for _, item := range messages {
					for _, call := range item.ToolCalls() {
						if call.ID == "matrix-call" {
							expectedID = session.CreateAgentToolSessionID(item.ID, call.ID)
						}
					}
				}
				require.Equal(t, expectedID, task.SessionID)
				assertRequest := func(marker string) {
					t.Helper()
					mu.Lock()
					captured := append([]string(nil), requests...)
					mu.Unlock()
					selected := ""
					for _, body := range captured {
						if strings.Contains(body, marker) && !strings.Contains(body, "LAUNCH_MATRIX_PARENT") {
							selected = body
						}
					}
					require.NotEmpty(t, selected, "no actual child provider request: %v", captured)
					require.Contains(t, selected, `"name":"read"`)
					if writer {
						require.Contains(t, selected, `"name":"write"`)
					} else {
						require.NotContains(t, selected, `"name":"write"`)
					}
					require.NotContains(t, selected, `"name":"bash"`)
					if named {
						require.Contains(t, selected, "MATRIX_SPECIALIZATION")
						require.Contains(t, selected, `"model":"selected"`)
						require.Contains(t, selected, "MATRIX_PRIOR_HISTORY")
					} else {
						require.NotContains(t, selected, "MATRIX_SPECIALIZATION")
						require.NotContains(t, selected, "MATRIX_PRIOR_HISTORY")
						require.Contains(t, selected, `"model":"parent"`)
					}
				}
				assertRequest("MATRIX_CHILD_GOAL")
				if isolation == "worktree" {
					require.NotEmpty(t, task.WorktreePath)
					require.NotEqual(t, repo, task.WorktreePath)
				} else {
					require.Empty(t, task.WorktreePath)
				}
				if writer && isolation == "worktree" {
					content, err := os.ReadFile(filepath.Join(task.WorktreePath, "matrix-written.txt"))
					require.NoError(t, err)
					require.Equal(t, "matrix write", string(content))
					require.NoFileExists(t, filepath.Join(repo, "matrix-written.txt"))
					require.NoFileExists(t, filepath.Join(repo, "matrix-parent-denied.txt"))
					output, err := boot.App.TaskManager().Output(t.Context(), task.ID, 100)
					require.NoError(t, err)
					require.NotEmpty(t, output.Messages)
					mu.Lock()
					deniedResponse := ""
					for _, body := range requests {
						if strings.Contains(body, `"role":"tool"`) && strings.Contains(body, "write-1") && !strings.Contains(body, "LAUNCH_MATRIX_PARENT") {
							deniedResponse = body
						}
					}
					mu.Unlock()
					require.NotEmpty(t, deniedResponse, "provider must receive actual denied write result")
					require.Contains(t, strings.ToLower(deniedResponse), "outside")
				}
				if named {
					boot.App.TaskManager().SweepIdleTasksForTest(t.Context(), time.Now().Add(24*time.Hour))
					_, err = boot.App.TaskManager().Send(t.Context(), task.ID, "MATRIX_FOLLOWUP")
					require.NoError(t, err)
					require.Eventually(t, func() bool {
						out, err := boot.App.TaskManager().Output(t.Context(), task.ID, 100)
						if err != nil {
							return false
						}
						for _, item := range out.Messages {
							if strings.Contains(item.Text, "MATRIX_FOLLOWUP") {
								return true
							}
						}
						return false
					}, 20*time.Second, 10*time.Millisecond)
					require.Eventually(t, func() bool {
						current, err := boot.App.TaskManager().Get(context.Background(), task.ID)
						return err == nil && current.Status == thread.StatusCompleted
					}, 20*time.Second, 10*time.Millisecond)
					assertRequest("MATRIX_FOLLOWUP")
					boot.App.Shutdown()
					restarted, err := app.Bootstrap(t.Context(), repo, app.BootstrapOptions{YOLO: true})
					require.NoError(t, err)
					defer restarted.App.Shutdown()
					Attach(t.Context(), restarted.App, repo, NewLocalSpawner(nil, nil, nil, nil))
					require.NoError(t, restarted.App.InitCoderAgentNonInteractive(t.Context()))
					_, err = restarted.App.TaskManager().Send(t.Context(), task.ID, "MATRIX_PROCESS_RESTART")
					require.NoError(t, err)
					require.Eventually(t, func() bool {
						out, err := restarted.App.TaskManager().Output(t.Context(), task.ID, 100)
						if err != nil {
							return false
						}
						for _, item := range out.Messages {
							if strings.Contains(item.Text, "MATRIX_PROCESS_RESTART") {
								return true
							}
						}
						return false
					}, 20*time.Second, 10*time.Millisecond)
					require.Eventually(t, func() bool {
						current, err := restarted.App.TaskManager().Get(t.Context(), task.ID)
						return err == nil && current.Status == thread.StatusCompleted
					}, 20*time.Second, 10*time.Millisecond)
					assertRequest("MATRIX_PROCESS_RESTART")
					current, err := restarted.App.TaskManager().Get(t.Context(), task.ID)
					require.NoError(t, err)
					require.Equal(t, task.SessionID, current.SessionID)
					require.Equal(t, task.CompletionDepth, current.CompletionDepth)
				}
			})
		}
	}
}
