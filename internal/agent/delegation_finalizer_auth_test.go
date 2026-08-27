package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/stretchr/testify/require"
)

// subAgentRotateEnvVar names the environment variable a delegate's
// api_key_template refresh re-resolves. Fixed rather than derived from
// t.Name() because shell variable names cannot contain the slashes a
// subtest name would introduce.
const subAgentRotateEnvVar = "SENNIT_TEST_SUBAGENT_ROTATE_KEY"

// TestRunSubAgent_RetryUsesRefreshedCredential pins the fix for a
// sub-agent retrying a 401 with the credential that just died: runSubAgent
// used to wire OnAuthRefresh with a nil *activeRuntime, so a refresh
// updated the live config but the in-flight turn kept dispatching through
// t.model - the provider instance built from the OLD credential when the
// delegate was constructed (see modelProvider in turn.go). This drives a
// real sub-agent through a real (httptest) provider endpoint that 401s the
// stale key once, and asserts the retry's Authorization header carries the
// freshly re-resolved key, not the one the first request sent.
//
// Before the fix (OnAuthRefresh wired with active=nil, no ActiveRuntime on
// the call) this test fails: the second request still carries the old key
// and the server 401s it again, so runSubAgent returns an error response
// instead of succeeding.
func TestRunSubAgent_RetryUsesRefreshedCredential(t *testing.T) {
	const oldKey = "old-key"
	const newKey = "new-key"
	t.Setenv(subAgentRotateEnvVar, oldKey)

	var (
		mu       sync.Mutex
		authz    []string
		sawFirst bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndReplaceBody(r)
		require.NoError(t, err)
		var payload struct {
			Tools json.RawMessage `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		if len(payload.Tools) == 0 {
			// Title generation carries no tools; keep it out of the
			// credential-rotation bookkeeping below, which only cares
			// about the delegate's own turn requests.
			sseStream(w, FixtureTurn{Text: "Test session"}, authModelID)
			return
		}

		mu.Lock()
		authz = append(authz, r.Header.Get("Authorization"))
		first := !sawFirst
		sawFirst = true
		mu.Unlock()

		if first {
			// The credential dies exactly here: by the time the refresh
			// callback re-resolves $SENNIT_TEST_SUBAGENT_ROTATE_KEY, the
			// value has rotated - simulating an external credential change
			// that made the key the sub-agent was built with dead.
			require.NoError(t, os.Setenv(subAgentRotateEnvVar, newKey))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`))
			return
		}
		sseStream(w, FixtureTurn{Text: "done after refresh"}, authModelID)
	}))
	defer srv.Close()

	co := authTestCoordinator(t, withProvider(func(p *config.ProviderConfig) {
		p.BaseURL = srv.URL + "/v1"
		p.APIKey = "$" + subAgentRotateEnvVar
		p.APIKeyTemplate = "$" + subAgentRotateEnvVar
	}))

	parent, err := co.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	agentCfg, ok := co.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "authTestCoordinator's SetupAgents must configure the task agent")

	p, err := taskPrompt(prompt.WithWorkingDir(co.cfg.WorkingDir()))
	require.NoError(t, err)
	delegate, err := co.delegation.newSubAgent(t.Context(), p, agentCfg)
	require.NoError(t, err)

	resp, err := co.delegation.runSubAgent(t.Context(), subAgentParams{
		Agent:          delegate,
		SessionID:      parent.ID,
		AgentMessageID: "msg-1",
		ToolCallID:     "call-1",
		Prompt:         "do something that needs a retry",
		SessionTitle:   "Test Session",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "delegation must succeed once the retry carries the refreshed credential: %s", resp.Content)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(authz), 2, "the 401 must have provoked a retry")
	require.Equal(t, "Bearer "+oldKey, authz[0], "the first attempt carries the credential the delegate was built with")
	require.Equal(t, "Bearer "+newKey, authz[len(authz)-1],
		"the retry must carry the refreshed credential, not the one that just got a 401")
}
