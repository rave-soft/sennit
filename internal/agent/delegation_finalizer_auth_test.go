package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
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
		p.APIKey = "$" + subAgentRotateEnvVar
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

// subAgentDifferentModelID names a second catalog model, distinct from
// authModelID, that only an agent's own agent.Model routes to - the
// coordinator's selected model never resolves to it.
const subAgentDifferentModelID = "sub-model-different"

// TestRunSubAgent_RetryUsesRefreshedCredential_DifferentModel pins the fix
// for makeAuthRefreshCallback storing the wrong runtime into active when a
// delegation's model differs from the coordinator's own: it used to always
// recompile the coder/top-level runtime (b.runtimeFor), which modelProvider
// (turn.go) only adopts when BOTH the stored runtime's provider AND model
// equal the turn's t.model. A custom agent running its own model therefore
// never matched, so the retry kept dispatching on t.model's stale
// provider instance - the exact case makeSubAgentAuthRefreshCallback fixes
// by rebuilding a runtime scoped to the delegation's own model instead.
//
// This drives a real custom-agent delegation, on a model different from the
// coordinator's selected one, through a real (httptest) provider endpoint
// that 401s the stale key once. It asserts both that the retry carries the
// refreshed credential and that every request - including the retry - names
// the sub-agent's own model, never the coder's.
func TestRunSubAgent_RetryUsesRefreshedCredential_DifferentModel(t *testing.T) {
	const oldKey = "old-key-different-model"
	const newKey = "new-key-different-model"
	t.Setenv(subAgentRotateEnvVar, oldKey)

	var (
		mu       sync.Mutex
		authz    []string
		models   []string
		sawFirst bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readAndReplaceBody(r)
		require.NoError(t, err)
		var payload struct {
			Tools json.RawMessage `json:"tools"`
			Model string          `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		if len(payload.Tools) == 0 {
			// Title generation; keep it out of the delegation's own
			// request bookkeeping below, same as the same-model test.
			sseStream(w, FixtureTurn{Text: "Test session"}, authModelID)
			return
		}

		mu.Lock()
		authz = append(authz, r.Header.Get("Authorization"))
		models = append(models, payload.Model)
		first := !sawFirst
		sawFirst = true
		mu.Unlock()

		if first {
			require.NoError(t, os.Setenv(subAgentRotateEnvVar, newKey))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error","code":"invalid_api_key"}}`))
			return
		}
		sseStream(w, FixtureTurn{Text: "done after refresh"}, subAgentDifferentModelID)
	}))
	defer srv.Close()

	co := authTestCoordinator(t, withProvider(func(p *config.ProviderConfig) {
		p.BaseURL = srv.URL + "/v1"
		p.APIKey = "$" + subAgentRotateEnvVar
		p.APIKey = "$" + subAgentRotateEnvVar
		// A second catalog model that only the custom agent below names,
		// so the delegation's own model genuinely differs from the
		// coordinator's selected authModelID.
		p.Models = append(p.Models, catwalkModelWithID(subAgentDifferentModelID))
	}))

	parent, err := co.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)

	// A custom agent naming its own model, distinct from the coordinator's
	// selected one - buildAgent resolves its configured model instead of
	// inheriting the app's main model (see TestBuildAgentCustomModel).
	taskCfg, ok := co.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "authTestCoordinator's SetupAgents must configure the task agent")
	agentCfg := taskCfg
	agentCfg.Model = authProviderID + "/" + subAgentDifferentModelID

	p, err := taskPrompt(prompt.WithWorkingDir(co.cfg.WorkingDir()))
	require.NoError(t, err)
	delegate, err := co.delegation.newSubAgent(t.Context(), p, agentCfg)
	require.NoError(t, err)
	require.Equal(t, subAgentDifferentModelID, delegate.Model().ModelCfg.Model,
		"the delegate must be built against its own agent.Model, not the coordinator's")

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
	for i, m := range models {
		require.Equal(t, subAgentDifferentModelID, m,
			"request %d must name the sub-agent's own model, not the coder's", i)
	}
}

// catwalkModelWithID returns a minimal catalog entry for id, for tests that
// only need a second model to exist in a provider's catalog.
func catwalkModelWithID(id string) catwalk.Model {
	return catwalk.Model{ID: id, DefaultMaxTokens: 4096}
}

// TestRunSubAgent_RateLimitRotatesAccountAndSucceeds is B4's end-to-end
// pin: before buildSubAgentCall wired OnRateLimit, a 429 mid-delegation had
// no rotation callback at all (buildSubAgentCall gave only OnAuthRefresh),
// so the delegation surfaced the original 429 instead of rotating to the
// other configured account the way a top-level turn would have. This
// drives a real sub-agent through a real (httptest) provider endpoint that
// 429s the first request and succeeds on the retry, with two accounts
// configured and rotation enabled, and asserts the retry actually carries
// the OTHER account's key.
func TestRunSubAgent_RateLimitRotatesAccountAndSucceeds(t *testing.T) {
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
			// delegation's own request bookkeeping below.
			sseStream(w, FixtureTurn{Text: "Test session"}, authModelID)
			return
		}

		mu.Lock()
		authz = append(authz, r.Header.Get("Authorization"))
		first := !sawFirst
		sawFirst = true
		mu.Unlock()

		if first {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
			return
		}
		sseStream(w, FixtureTurn{Text: "done after rotation"}, authModelID)
	}))
	defer srv.Close()

	co := authTestCoordinator(t, withProvider(func(p *config.ProviderConfig) {
		p.BaseURL = srv.URL + "/v1"
		p.APIKey = "key-a"
		p.Account = "acct-a"
		p.Rotation = &config.RotationConfig{Enabled: true}
	}))
	co.builder.accountsStore = newFakeAccountStore(authProviderID,
		apiKeyAccount("acct-a", "key-a"),
		apiKeyAccount("acct-b", "key-b"),
	)

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
		Prompt:         "do something that hits a rate limit",
		SessionTitle:   "Test Session",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "delegation must succeed once the retry rotates onto the other account: %s", resp.Content)

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(authz), 2, "the 429 must have provoked a retry")
	require.Equal(t, "Bearer key-a", authz[0], "the first attempt carries the account the delegate was built with")
	require.Equal(t, "Bearer key-b", authz[len(authz)-1],
		"the retry must carry the OTHER account's key, i.e. the delegation actually rotated")

	after, ok := co.cfg.Config().RuntimeProvider(authProviderID)
	require.True(t, ok)
	require.Equal(t, "acct-b", after.Account, "rotation must have activated the other account globally")
}
