package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/configruntime"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/stretchr/testify/require"
)

// TestCoordinatorCloseRaceWithBuildAgent is a regression test for a
// -race panic found in production use: concurrent buildAgent calls (sub-
// agent rebuilds happen on every coordinator.Run, and strands can dispatch
// several Run calls at once on the same coordinator) raced Close's
// readyGroup.Wait against a later buildAgent's readyGroup.Add — "sync:
// WaitGroup is reused before previous Wait has returned". buildAgent now
// gates its Add on Close's closing flag under readyMu, and Close runs
// readyGroup.Wait at most once (via closeOnce) no matter how many callers
// invoke it concurrently. Run with -race -count=N; a regression panics the
// test binary rather than failing an assertion.
func TestCoordinatorCloseRaceWithBuildAgent(t *testing.T) {
	env := testEnv(t)

	// Minimal hermetic config: buildAgent only needs a resolvable
	// model to get past buildAgentModel; no real network
	// call happens before the readiness goroutines are already spawned.
	sennitJSON := `{
  "options": {"disable_default_providers": true},
  "providers": {"mock": {"id": "mock", "name": "Mock", "type": "openai",
    "base_url": "http://127.0.0.1:9/v1", "api_key": "test-key",
    "models": [{"id": "mock-model", "name": "Mock", "context_window": 8192, "default_max_tokens": 128}]}},
  "models": {"large": {"provider": "mock", "model": "mock-model"},
             "small": {"provider": "mock", "model": "mock-model"}}
}`
	writeGlobalConfig(t, sennitJSON)

	cfg, err := configruntime.Load(env.workingDir, "", false)
	require.NoError(t, err)
	cfg.SetupAgents()

	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		history:     env.history,
		filetracker: *env.filetracker,
		mcp:         mcp.NewRegistry(),
		background:  shell.NewBackgroundShellManager(),
	}

	p, err := coderPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)
	agentCfg := cfg.Config().Agents[config.AgentCoder]

	const buildAgentCalls = 30

	var wg sync.WaitGroup
	wg.Add(buildAgentCalls)
	for range buildAgentCalls {
		go func() {
			defer wg.Done()
			// isSubAgent=true: this is the path that rebuilds on every
			// run and, before the fix, shared readyGroup unprotected.
			_, _ = coord.buildAgent(context.Background(), p, agentCfg, true)
		}()
	}

	// Two overlapping Close calls exercise both the closing-flag gate in
	// buildAgent and the closeOnce idempotency in Close itself.
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = coord.Close(context.Background())
	}()
	go func() {
		defer wg.Done()
		_ = coord.Close(context.Background())
	}()

	wg.Wait()
}
