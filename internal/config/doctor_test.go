package config

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/stretchr/testify/require"
)

// setupUserAgentsForTest runs the validate-and-rebuild step SetupAgents
// performs on whatever is already in cfg.Agents, without SetupAgents' own
// unconditional overwrite of cfg.Agents from markdown discovery. Since
// SetupAgents now discovers agents from .sennit/agents/*.md only (see
// config.go), a Config built by hand in these tests (no workingDir, no
// files on disk) has nothing to discover; this lets the doctor tests below
// still exercise validUserAgents' model-resolution fallback and Problem
// bookkeeping against a hand-built Agent map, the same way SetupAgents
// itself invokes it.
func setupUserAgentsForTest(cfg *Config) {
	cfg.Problems = slices.DeleteFunc(cfg.Problems, func(p Problem) bool { return p.Area == AreaAgent })
	valid, _ := cfg.validUserAgents()
	cfg.Agents = valid
}

// doctorTestConfig builds a Config with one provider ("openai") offering a
// reasoning model ("o1") and a non-reasoning one ("gpt-4o-mini"), ready for
// SetupAgents.
func doctorTestConfig(t *testing.T) *Config {
	t.Helper()
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai", ProviderConfig{
		ID: "openai",
		Models: []catwalk.Model{
			{ID: "o1", CanReason: true},
			{ID: "gpt-4o-mini"},
		},
	})
	return &Config{
		Options:   &Options{},
		Providers: providers,
		Model:     SelectedModel{Provider: "openai", Model: "gpt-4o-mini"},
	}
}

func TestDoctorNilConfig(t *testing.T) {
	require.Empty(t, Doctor(nil))
}

func TestDoctorCleanConfig(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

// TestDoctorAgentUnresolvedModel covers "agent with model not resolving to
// a provider/model" — the motivating case from the feature request: a
// sub-agent pinned to a model that doesn't exist should surface as a
// Problem, not just a log line.
func TestDoctorAgentUnresolvedModel(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "x/y"},
	}
	setupUserAgentsForTest(cfg)

	problems := Doctor(cfg)
	require.NotEmpty(t, problems)
	found := false
	for _, p := range problems {
		if p.Area == AreaAgent && p.Subject == "reviewer" {
			found = true
			require.Equal(t, SeverityWarn, p.Severity)
			require.Contains(t, p.Message, "reviewer")
			require.Contains(t, p.Message, "x/y")
			require.Contains(t, p.Message, "falls back to the main model")
		}
	}
	require.True(t, found, "expected a problem for the reviewer agent's unresolved model")

	// The reviewer agent itself must still work (fallback, not rejection).
	reviewer, ok := cfg.Agents["reviewer"]
	require.True(t, ok)
	require.Empty(t, reviewer.Model)
}

// TestDoctorAgentUnresolvedModel_ClearedOnFix verifies SetupAgents does not
// leave a stale Problem behind once the offending definition is fixed and
// SetupAgents runs again on the same live Config.
func TestDoctorAgentUnresolvedModel_ClearedOnFix(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "x/y"},
	}
	setupUserAgentsForTest(cfg)
	require.NotEmpty(t, Doctor(cfg))

	cfg.Agents["reviewer"] = Agent{Prompt: "You review code.", Model: "openai/o1"}
	setupUserAgentsForTest(cfg)
	require.Empty(t, Doctor(cfg))
}

func TestDoctorAgentReasoningEffortUnsupported(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "openai/gpt-4o-mini", ReasoningEffort: "high"},
	}
	setupUserAgentsForTest(cfg)

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaAgent, problems[0].Area)
	require.Equal(t, SeverityWarn, problems[0].Severity)
	require.Contains(t, problems[0].Message, "does not support reasoning")
}

func TestDoctorAgentReasoningEffortSupported(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", Model: "openai/o1", ReasoningEffort: "high"},
	}
	setupUserAgentsForTest(cfg)

	require.Empty(t, Doctor(cfg))
}

func TestDoctorAgentReasoningEffortInheritsMainModel(t *testing.T) {
	// No agent.Model set: the agent inherits cfg.Model, which is pinned to
	// the non-reasoning gpt-4o-mini in doctorTestConfig.
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", ReasoningEffort: "low"},
	}
	setupUserAgentsForTest(cfg)

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Message, "does not support reasoning")
}

func TestDoctorDisabledToolsUnknownName(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Options.DisabledTools = []string{"bash", "totally_not_a_tool"}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaAgent, problems[0].Area)
	require.Contains(t, problems[0].Message, "totally_not_a_tool")
}

func TestDoctorAllowedToolsUnknownName(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", AllowedTools: []string{"view", "not_a_real_tool"}},
	}
	setupUserAgentsForTest(cfg)

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, "reviewer", problems[0].Subject)
	require.Contains(t, problems[0].Message, "not_a_real_tool")
}

// TestDoctorAllowedToolsAgentPseudoToolKnown verifies that a coder-delegate
// agent id (a legitimate pseudo-tool name) is not flagged.
func TestDoctorAllowedToolsAgentPseudoToolKnown(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code."},
		"planner":  {Prompt: "You plan.", AllowedTools: []string{"view", "reviewer"}},
	}
	setupUserAgentsForTest(cfg)

	require.Empty(t, Doctor(cfg))
}

// TestDoctorAllowedToolsMCPPrefixSkipped verifies mcp_ prefixed tool names
// are not validated statically (MCP tool names are only known once the
// registry has connected).
func TestDoctorAllowedToolsMCPPrefixSkipped(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Agents = map[string]Agent{
		"reviewer": {Prompt: "You review code.", AllowedTools: []string{"mcp_context7_query-docs"}},
	}
	setupUserAgentsForTest(cfg)

	require.Empty(t, Doctor(cfg))
}

// TestDoctorPermissionsBypassEnabled verifies a persistent permissions.bypass
// surfaces as a warning, since it silently disables every permission prompt
// for the life of the process.
func TestDoctorPermissionsBypassEnabled(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Permissions = &Permissions{Bypass: true}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaPermission, problems[0].Area)
	require.Equal(t, SeverityWarn, problems[0].Severity)
	require.Equal(t, "permissions.bypass", problems[0].Subject)
	require.Contains(t, problems[0].Message, "never asks for permission")
}

// TestDoctorPermissionsBypassDisabled covers both the explicit-false and
// unset (nil Permissions) cases: neither should produce a Problem.
func TestDoctorPermissionsBypassDisabled(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.SetupAgents()
	require.Empty(t, Doctor(cfg))

	cfg.Permissions = &Permissions{Bypass: false}
	require.Empty(t, Doctor(cfg))
}

// TestDoctorProviderMissingAPIKey covers a custom provider that survives
// config load (local providers legitimately have no key) but is still
// worth flagging.
func TestDoctorProviderMissingAPIKey(t *testing.T) {
	cfg := &Config{Options: &Options{}, Problems: []Problem{
		{Severity: SeverityWarn, Area: AreaProvider, Subject: "local", Message: "provider local has no api_key"},
	}}
	cfg.SetupAgents() // must not drop provider-area problems.

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaProvider, problems[0].Area)
	require.Equal(t, "local", problems[0].Subject)
}

// TestDoctorJunkModelIDDiscovered covers the case the check exists for: a
// gguf path that reached a discovering provider's model list from a
// llama.cpp /v1/models response.
func TestDoctorJunkModelIDDiscovered(t *testing.T) {
	discover := true
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("local", ProviderConfig{
		ID:                 "local",
		AutoDiscoverModels: &discover,
		Models:             []catwalk.Model{{ID: "/models/Qwen3.8-27B/Qwen3.8-27B-Q8_0.gguf"}},
	})
	cfg := &Config{Options: &Options{}, Providers: providers}

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaProvider, problems[0].Area)
	require.Equal(t, "local", problems[0].Subject)
	require.Contains(t, problems[0].Message, "looks like a file path")
}

// TestDoctorJunkModelIDExplicitProviderSkipped guards the qwen36-local
// case: with discover_models: false the model list is hand-written, and a
// llama-server's only accepted model ID really is the --model path. The
// user has already done what the hint asks, so warning again is noise with
// no remedy.
func TestDoctorJunkModelIDExplicitProviderSkipped(t *testing.T) {
	discover := false
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("qwen36-local", ProviderConfig{
		ID:                 "qwen36-local",
		AutoDiscoverModels: &discover,
		Models:             []catwalk.Model{{ID: "/models/Qwen3.8-27B/Qwen3.8-27B-Q8_0.gguf"}},
	})
	cfg := &Config{Options: &Options{}, Providers: providers}

	require.Empty(t, Doctor(cfg))
}

// TestDoctorReportsJSONAgentsBlock is the end-to-end version of
// TestSetupAgentsIgnoresJSONAgentsBlock: it loads a real sennit.json with an
// "agents" block off disk through loadFromConfigPaths (not a hand-built
// Config) and asserts the ignored block surfaces through Doctor, the same
// list `sennit doctor` and the TUI's /doctor dialog render.
func TestDoctorReportsJSONAgentsBlock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sennit.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"agents":{"reviewer":{"prompt":"You review code."}}}`), 0o644))

	cfg, _, err := loadFromConfigPaths(context.Background(), []string{path}, true)
	require.NoError(t, err)
	cfg.Options = &Options{}
	cfg.workingDir = root
	cfg.SetupAgents()

	problems := Doctor(cfg)
	found := false
	for _, p := range problems {
		if p.Area == AreaAgent && p.Severity == SeverityWarn && p.Subject == "agents" {
			found = true
			require.Contains(t, p.Message, "agents in sennit.json are ignored — define agents as .sennit/agents/*.md files")
		}
	}
	require.True(t, found, "expected a Problem for the ignored JSON agents block")
	require.NotContains(t, cfg.Agents, "reviewer", "the JSON agent must never be read")
}

// TestDoctorMainModelFallback exercises resolveSelectedModel's fallback
// path end to end: an unresolvable configured main model must both fall
// back (existing behavior) and be recorded as a Problem (new behavior).
func TestDoctorMainModelFallback(t *testing.T) {
	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("openai", ProviderConfig{
		ID:     "openai",
		Models: []catwalk.Model{{ID: "gpt-4o-mini", DefaultMaxTokens: 4096}},
	})
	cfg := &Config{
		Options:   &Options{},
		Providers: providers,
		Model:     SelectedModel{Provider: "openai", Model: "does-not-exist"},
	}

	knownProviders := []catwalk.Provider{{
		ID:                  catwalk.InferenceProviderOpenAI,
		DefaultLargeModelID: "gpt-4o-mini",
		Models:              []catwalk.Model{{ID: "gpt-4o-mini", DefaultMaxTokens: 4096}},
	}}
	resolved, err := resolveSelectedModel(cfg, knownProviders)
	require.NoError(t, err)
	require.True(t, resolved.Fallback)
	cfg.Model = resolved.Model

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, AreaModel, problems[0].Area)
	require.Equal(t, SeverityError, problems[0].Severity)
	require.Contains(t, problems[0].Message, "does-not-exist")
}

// TestDoctorExcludesEnvironmentProblems pins the split: Doctor answers
// "is this config right?" from the config alone, so it stays reproducible
// on a machine that happens to lack a clipboard helper.
func TestDoctorExcludesEnvironmentProblems(t *testing.T) {
	t.Setenv("PATH", "")

	cfg := doctorTestConfig(t)
	cfg.SetupAgents()

	require.Empty(t, Doctor(cfg))
}

// TestDoctorPermissionsAllowedToolsUnknownName covers the third list of
// tool names a config can carry, and the one a typo hurts most: an entry
// that matches nothing grants nothing, so the person keeps answering the
// prompt they believe they turned off. The documentation has promised
// this check for a while; only disabled_tools and an agent's
// allowed_tools were actually wired up.
func TestDoctorPermissionsAllowedToolsUnknownName(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Permissions = &Permissions{AllowedTools: []string{"read", "reed"}}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Equal(t, "permissions.allowed_tools", problems[0].Subject)
	require.Contains(t, problems[0].Message, "reed")
}

// TestDoctorPermissionsAllowedToolsWithAction pins both halves of an
// entry that names an action. The action comes from a closed vocabulary
// (permission.KnownActions) because Request builds its key by joining the
// tool name and the action, so "bash:execute" is a rule and "bash:npm run
// build" reads like one while matching nothing at all - the same silent
// nothing a misspelled tool name produces, which is what this check
// exists to surface.
func TestDoctorPermissionsAllowedToolsWithAction(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Permissions = &Permissions{AllowedTools: []string{"bash:execute", "bash:npm run build"}}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Message, "bash:npm run build")
	require.Contains(t, problems[0].Hint, "execute")
}

// TestDoctorPermissionsAllowedToolsUnknownToolBeatsAction keeps the two
// halves from being reported twice for one entry: a misspelled tool is
// the more useful thing to say.
func TestDoctorPermissionsAllowedToolsUnknownToolBeatsAction(t *testing.T) {
	cfg := doctorTestConfig(t)
	cfg.Permissions = &Permissions{AllowedTools: []string{"bash2:whatever"}}
	cfg.SetupAgents()

	problems := Doctor(cfg)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Message, "bash2")
}
