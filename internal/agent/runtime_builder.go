package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/question"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
)

// runtimeBuilder owns the per-turn runtime construction and resolution:
// the compiled-runtime cache and its local invalidation generation, the
// model/provider/tool/prompt assembly that fills it, and the credential
// refresh paths (OAuth, AWS SSO, API-key templates) that invalidate it and
// re-resolve models. It is the single place "what will this run execute
// with" is answered, and the single place "the credentials changed" is
// acted on.
//
// It holds no session or turn state: nothing here knows which session is
// running, and a build never takes a dispatch lock. The readiness
// goroutines that call into it (see turnDispatcher.buildAgent) are started
// by the dispatcher, not by this component.
type runtimeConfigSnapshot struct {
	config                  *config.Config
	resolver                config.VariableResolver
	workingDir              string
	overrides               config.RuntimeOverrides
	loadedPaths             []string
	staleness               config.StalenessResult
	reserveMCPTokenMutation func(string, config.MCPConfig) (config.MCPTokenMutation, bool)
	setMCPToken             func(context.Context, *config.MCPTokenMutation, *oauth.Token) (bool, error)
	clearMCPToken           func(*config.MCPTokenMutation, *oauth.Token) (bool, error)
	// activeSkills is the coordinator's own computed skill list, handed
	// to the prompt so it stops recomputing one from disk. See
	// ActiveSkills.
	activeSkills []*skills.Skill
}

// ActiveSkills implements prompt.SkillsProvider. Without it the prompt
// rediscovers skills by walking the configured directories, which misses
// the inherited ones entirely - and a thread lives in a git worktree with
// no .sennit/skills of its own, which is exactly why inheritance exists.
// The result was a thread whose <available_skills> omitted every project
// skill while sennit_info, reading the same coordinator's list, reported
// them active. The two answers come from one list now.
func (s runtimeConfigSnapshot) ActiveSkills() []*skills.Skill {
	return s.activeSkills
}

func (s runtimeConfigSnapshot) Config() *config.Config {
	return s.config
}

func (s runtimeConfigSnapshot) Resolver() config.VariableResolver {
	return s.resolver
}

func (s runtimeConfigSnapshot) WorkingDir() string {
	return s.workingDir
}

func (s runtimeConfigSnapshot) Overrides() config.RuntimeOverrides {
	return s.overrides
}

func (s runtimeConfigSnapshot) LoadedPaths() []string {
	return slices.Clone(s.loadedPaths)
}

func (s runtimeConfigSnapshot) ConfigStaleness() config.StalenessResult {
	return s.staleness
}

func (s runtimeConfigSnapshot) ReserveMCPTokenMutation(name string, expected config.MCPConfig) (config.MCPTokenMutation, bool) {
	return s.reserveMCPTokenMutation(name, expected)
}

func (s runtimeConfigSnapshot) SetMCPTokenContext(ctx context.Context, reservation *config.MCPTokenMutation, token *oauth.Token) (bool, error) {
	return s.setMCPToken(ctx, reservation, token)
}

func (s runtimeConfigSnapshot) ClearMCPToken(reservation *config.MCPTokenMutation, expectedToken *oauth.Token) (bool, error) {
	return s.clearMCPToken(reservation, expectedToken)
}

type runtimeToolInputs struct {
	allSkills, activeSkills []*skills.Skill
	skillTracker            *skills.Tracker
	delegationTools         delegationToolsSnapshot
	backgroundAgentsOn      bool
	permissions             permission.Requester
	delegationToolsBuilt    map[string]fantasy.AgentTool
	toolBuildErr            error
	questions               question.Service
	lspManager              *lsp.Manager
	fileHistory             tools.FileHistory
	filetracker             tools.FileTracking
	background              *shell.BackgroundShellManager
	sessions                sessionstore.Service
	skillStates             []*skills.SkillState
}

type runtimeBuilder struct {
	*agentDeps

	localVersion atomic.Uint64
	runtime      *runtimeCache

	// runtimeInvalidationMu serializes runtime-affecting state mutation and
	// publication of each local generation with its exact invalidation reason.
	// Runtime builds never hold it.
	runtimeInvalidationMu sync.Mutex

	// rotatorsMu guards rotators. Plain map + mutex, not csync.Map: the
	// map is built lazily and every test that constructs a bare
	// &runtimeBuilder{} (see runtime_builder_test.go and friends) must
	// keep working with rotators left at its nil zero value, which a
	// csync.Map field would not survive uninitialized.
	rotatorsMu sync.Mutex
	// rotators holds one *accounts.Rotator per provider, for the
	// process's lifetime: cooldown state (accounts.Rotator's doc
	// comment) lives only in memory and must survive across requests, so
	// it cannot be rebuilt per-turn the way a compiledRuntime is. Built
	// lazily by rotatorFor, and only for providers with rotation
	// enabled - see rotatorFor for why a disabled provider never gets an
	// entry here at all.
	rotators map[string]*accounts.Rotator
}

// waitForMCPInit blocks until this builder's MCP registry finishes
// initializing. A builder built without a registry (a handful of tests
// construct one directly) has nothing to wait for.
func (b *runtimeBuilder) waitForMCPInit(ctx context.Context) error {
	if b.mcp == nil {
		return nil
	}
	return b.mcp.WaitForInit(ctx)
}

func (b *runtimeBuilder) mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatalogCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatalogCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatalogCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatalogCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatalogCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

// buildTools assembles the tool set an agent build gets, from toolSpecs
// (tool_registry.go) plus two groups that can't be fixed rows there: the
// user-defined agent tools (one per config.Agents entry — never offered
// to a sub-agent, or delegation could recurse without bound) and the
// per-MCP-server tools (gated by AllowedMCP, not AllowedTools).
//
// b.cfg.Config() is read exactly once, into an agentConfig snapshot (see
// newAgentConfig): ConfigStore.Config() takes no lock spanning multiple
// calls, so reading it repeatedly across one build let a concurrent
// config reload hand different tools in the same set different values.
// One snapshot means every tool here sees the same config.
func (b *runtimeBuilder) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool, inputs runtimeToolInputs) ([]fantasy.AgentTool, error) {
	return b.buildToolsForConfig(ctx, agent, isSubAgent, inputs, b.runtimeConfigSnapshot())
}

func (b *runtimeBuilder) buildToolsForConfig(ctx context.Context, agent config.Agent, isSubAgent bool, inputs runtimeToolInputs, snapshot runtimeConfigSnapshot) ([]fantasy.AgentTool, error) {
	if inputs.toolBuildErr != nil {
		return nil, inputs.toolBuildErr
	}
	cfg := newAgentConfig(snapshot.config)

	bctx, err := b.newBuildToolsCtx(cfg, snapshot, agent, isSubAgent, inputs)
	if err != nil {
		return nil, err
	}

	allTools, err := b.assembleAllTools(ctx, bctx)
	if err != nil {
		return nil, err
	}

	filteredTools := filterToolsByAllowlist(allTools, agent)
	filteredTools = appendAllowedMCPTools(filteredTools, b, inputs, agent, snapshot)
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := cfg.PreToolUseHooks(); len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, snapshot.workingDir, snapshot.workingDir)
	}

	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)

	return filteredTools, nil
}

// newBuildToolsCtx assembles the buildToolsCtx buildTools' toolSpecs loop
// gates and builds against, from the current web-search backend and the
// already-collected runtime inputs (skills, delegation tools, background
// agents).
func (b *runtimeBuilder) newBuildToolsCtx(cfg agentConfig, snapshot runtimeConfigSnapshot, agent config.Agent, isSubAgent bool, inputs runtimeToolInputs) (*buildToolsCtx, error) {
	searchBackend, err := webSearchBackend(snapshot)
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}

	allSkillsSnapshot, activeSkillsSnapshot, skillTrackerSnapshot := inputs.allSkills, inputs.activeSkills, inputs.skillTracker
	delegationTools := inputs.delegationTools
	return &buildToolsCtx{
		agent:              agent,
		isSubAgent:         isSubAgent,
		interactive:        b.interactive,
		cfg:                cfg,
		modelID:            cfg.ModelID(),
		logFile:            config.GlobalLogFile(),
		searchBackend:      searchBackend,
		allSkills:          allSkillsSnapshot,
		activeSkills:       activeSkillsSnapshot,
		skillTracker:       skillTrackerSnapshot,
		threads:            delegationTools.threads,
		taskManager:        delegationTools.tasks,
		backgroundAgentsOn: inputs.backgroundAgentsOn,
		toolAvailability:   tools.ResolveSystemToolAvailability(),
		inputs:             inputs,
		runtimeCfg:         snapshot,
	}, nil
}

// assembleAllTools runs every gated toolSpecs() entry against bctx and
// appends the pre-built user-defined agent tools for the top-level agent
// only (user-defined agents are offered to the top-level agent only - see
// buildTools' doc comment).
func (b *runtimeBuilder) assembleAllTools(ctx context.Context, bctx *buildToolsCtx) ([]fantasy.AgentTool, error) {
	var allTools []fantasy.AgentTool
	for _, spec := range toolSpecs() {
		gate, ok := specGate(spec)
		if !ok || !gateAllows(gate, spec.Names[0], bctx) {
			continue
		}
		built, err := spec.Build(ctx, b, bctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, built...)
	}
	return allTools, nil
}

// filterToolsByAllowlist keeps only the tools agent.AllowedTools names.
// grep and ripgrep are alternative registrations of the same content
// search slot (which one exists depends on whether rg is installed), so
// an agent allowing either name gets whichever is available.
func filterToolsByAllowlist(allTools []fantasy.AgentTool, agent config.Agent) []fantasy.AgentTool {
	allowsTool := func(name string) bool {
		if name == tools.GrepToolName || name == tools.RipgrepToolName {
			return slices.Contains(agent.AllowedTools, tools.GrepToolName) ||
				slices.Contains(agent.AllowedTools, tools.RipgrepToolName)
		}
		return slices.Contains(agent.AllowedTools, name)
	}
	var filteredTools []fantasy.AgentTool
	for _, tool := range allTools {
		if allowsTool(tool.Info().Name) {
			filteredTools = append(filteredTools, tool)
		}
	}
	return filteredTools
}

// appendAllowedMCPTools appends the MCP tools agent.AllowedMCP permits (nil
// means no restrictions; an empty, non-nil map means none allowed) to
// filteredTools.
func appendAllowedMCPTools(filteredTools []fantasy.AgentTool, b *runtimeBuilder, inputs runtimeToolInputs, agent config.Agent, snapshot runtimeConfigSnapshot) []fantasy.AgentTool {
	for _, tool := range tools.GetMCPTools(b.mcp, inputs.permissions, snapshot, snapshot.workingDir) {
		if agent.AllowedMCP == nil {
			// No MCP restrictions
			filteredTools = append(filteredTools, tool)
			continue
		}
		if len(agent.AllowedMCP) == 0 {
			// No MCPs allowed
			slog.Debug("No MCPs allowed", "tool", tool.Name(), "agent", agent.Name)
			break
		}

		for mcp, toolNames := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(toolNames) == 0 || slices.Contains(toolNames, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	return filteredTools
}

// webSearchBackend builds the SearchBackend selected by options.web_search,
// defaulting to the keyless DuckDuckGo scraper when the section is absent.
// api_key and proxy_url run through the same shell-expansion resolver used
// for provider api_key/proxy_url.
func (b *runtimeBuilder) webSearchBackend() (tools.SearchBackend, error) {
	return webSearchBackend(b.runtimeConfigSnapshot())
}

func webSearchBackend(snapshot runtimeConfigSnapshot) (tools.SearchBackend, error) {
	var opts config.WebSearchOptions
	if ws := snapshot.config.Options.WebSearch; ws != nil {
		opts = *ws
	}
	return tools.NewSearchBackend(opts, snapshot.resolver, nil)
}

// buildAgentModel resolves one agent's model from a single runtime snapshot.
// An empty Agent.Model inherits the application's selected model; otherwise the
// agent's existing provider/model-id setting is resolved against that snapshot.
func (b *runtimeBuilder) buildAgentModel(ctx context.Context, agent config.Agent, isSubAgent bool) (Model, error) {
	return b.buildAgentModelForSnapshot(ctx, b.runtimeConfigSnapshot(), agent, isSubAgent)
}

func (b *runtimeBuilder) buildAgentModelForSnapshot(ctx context.Context, snapshot runtimeConfigSnapshot, agent config.Agent, isSubAgent bool) (Model, error) {
	runtimeCfg := snapshot.config
	selected := runtimeCfg.Model
	if agent.Model != "" {
		match, err := config.ResolveModelString(runtimeCfg.Providers.Copy(), agent.Model)
		if err != nil {
			return Model{}, fmt.Errorf("agent %q model %q: %w", agent.Name, agent.Model, err)
		}
		selected = config.SelectedModel{Provider: match.Provider, Model: match.ModelID}
	}
	if selected.Model == "" {
		return Model{}, errModelNotSelected
	}

	providerCfg, ok := runtimeCfg.Providers.Get(selected.Provider)
	if !ok {
		if agent.Model != "" {
			return Model{}, fmt.Errorf("agent %q model %q: provider %q not configured", agent.Name, agent.Model, selected.Provider)
		}
		return Model{}, errModelProviderNotConfigured
	}

	model, err := b.buildModelForSnapshot(ctx, providerCfg, selected, isSubAgent, snapshot)
	if agent.Model != "" && errors.Is(err, errModelNotFound) {
		return Model{}, fmt.Errorf("agent %q model %q: model not found in provider config", agent.Name, agent.Model)
	}
	return model, err
}

// buildModel resolves selected's provider, catalog entry, and language
// model, and assembles the result. Shared by buildAgentModel (the app's
// selected model, whether inherited or named by an agent: both need the
// same provider-build -> catalog-lookup -> openrouter ":exacto" suffix ->
// language-model steps. It reports a bare errModelNotFound so agent-specific
// resolution can add the configured agent and model to the error.
func (b *runtimeBuilder) buildModel(ctx context.Context, providerCfg config.ProviderConfig, selected config.SelectedModel, isSubAgent bool) (Model, error) {
	return b.buildModelForSnapshot(ctx, providerCfg, selected, isSubAgent, b.runtimeConfigSnapshot())
}

func (b *runtimeBuilder) buildModelForSnapshot(ctx context.Context, providerCfg config.ProviderConfig, selected config.SelectedModel, isSubAgent bool, snapshot runtimeConfigSnapshot) (Model, error) {
	provider, err := b.buildProviderForSnapshot(providerCfg, selected, isSubAgent, snapshot)
	if err != nil {
		return Model{}, err
	}

	catalogModel := findCatalogModel(providerCfg, selected.Model)
	if catalogModel == nil {
		return Model{}, errModelNotFound
	}

	modelID := selected.Model
	if selected.Provider == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	return Model{
		Model:      languageModel,
		CatalogCfg: *catalogModel,
		ModelCfg:   selected,
		FlatRate:   providerCfg.FlatRate,
	}, nil
}

// findCatalogModel returns the first entry in providerCfg.Models whose ID
// matches modelID, or nil if none matches. First match wins: if a
// provider's Models ever ends up with two entries sharing an ID (a
// duplicate that nothing today rejects at config-load time), the earlier
// entry is authoritative rather than whichever happened to be seen last —
// deterministic and matching how ResolveModelString itself expects a model
// ID to name exactly one entry.
func findCatalogModel(providerCfg config.ProviderConfig, modelID string) *catwalk.Model {
	for _, m := range providerCfg.Models {
		if m.ID == modelID {
			return &m
		}
	}
	return nil
}

func isExactoSupported(modelID string) bool {
	supportedModels := []string{
		"moonshotai/kimi-k2-0905",
		"deepseek/deepseek-v3.1-terminus",
		"z-ai/glm-4.6",
		"openai/gpt-oss-120b",
		"qwen/qwen3-coder",
	}
	return slices.Contains(supportedModels, modelID)
}

func (b *runtimeBuilder) runtimeKey() runtimeKey {
	key := runtimeKey{local: b.localVersion.Load()}
	if b.cfg != nil {
		key.config = b.cfg.Version()
	}
	if b.mcp != nil {
		key.mcp = b.mcp.Version()
	}
	return key
}

func (b *runtimeBuilder) invalidateRuntime(ctx context.Context, reason string, mutate func() bool) {
	b.runtimeInvalidationMu.Lock()
	defer b.runtimeInvalidationMu.Unlock()
	if !mutate() {
		return
	}
	nextVersion := b.localVersion.Load() + 1
	nextKey := b.runtimeKey()
	nextKey.local = nextVersion
	if b.runtime != nil {
		b.runtime.invalidateAndPublish(ctx, reason, nextKey, func() {
			b.localVersion.Store(nextVersion)
		})
		return
	}
	b.localVersion.Store(nextVersion)
}

func (b *runtimeBuilder) runtimeConfigSnapshot() runtimeConfigSnapshot {
	published := b.cfg.RuntimeSnapshot()
	return runtimeConfigSnapshot{
		config:                  published.Config,
		resolver:                published.Resolver,
		workingDir:              published.WorkingDir,
		overrides:               published.Overrides,
		loadedPaths:             published.LoadedPaths,
		staleness:               published.Staleness,
		reserveMCPTokenMutation: b.cfg.ReserveMCPTokenMutation,
		setMCPToken:             b.cfg.SetMCPTokenContext,
		clearMCPToken:           b.cfg.ClearMCPToken,
	}
}

func (b *runtimeBuilder) runtimeFor(ctx context.Context, inputs runtimeToolInputs) (*compiledRuntime, error) {
	return b.runtime.getOrBuild(ctx, b.runtimeKey, func(ctx context.Context, key runtimeKey) (*compiledRuntime, error) {
		runtimeCfg := b.runtimeConfigSnapshot()
		agentCfg, ok := runtimeCfg.config.Agents[config.AgentCoder]
		if !ok {
			return nil, errCoderAgentNotConfigured
		}
		model, err := b.buildAgentModelForSnapshot(ctx, runtimeCfg, agentCfg, false)
		if err != nil {
			return nil, err
		}
		builtTools, err := b.buildToolsForConfig(ctx, agentCfg, false, inputs, runtimeCfg)
		if err != nil {
			return nil, err
		}
		runtimePrompt, err := coderPrompt(prompt.WithWorkingDir(runtimeCfg.workingDir))
		if err != nil {
			return nil, err
		}
		// Attached here rather than in runtimeConfigSnapshot: the active
		// list belongs to this build's inputs, not to the published
		// config the snapshot is otherwise made of.
		runtimeCfg.activeSkills = inputs.activeSkills
		systemPrompt, err := runtimePrompt.Build(ctx, model.Model.Provider(), model.Model.Model(), runtimeCfg)
		if err != nil {
			return nil, err
		}
		if len(builtTools) > 0 {
			builtTools[len(builtTools)-1].SetProviderOptions(cacheControlOptions())
		}
		providerCfg, ok := runtimeCfg.config.Providers.Get(model.ModelCfg.Provider)
		if !ok {
			return nil, errModelProviderNotConfigured
		}
		providerCredentials, ok := runtimeCfg.config.RuntimeProvider(model.ModelCfg.Provider)
		if !ok {
			return nil, errModelProviderNotConfigured
		}
		options, temp, topP, topK, freqPenalty, presPenalty := b.mergeCallOptions(model, providerCfg)
		maxTokens := modelMaxOutputTokens(model)
		return &compiledRuntime{
			key: key, model: model, tools: builtTools, systemPrompt: systemPrompt,
			providerCfg: providerCfg, providerCredentials: providerCredentials, providerOptions: options,
			temperature: temp, topP: topP, topK: topK,
			frequencyPenalty: freqPenalty, presencePenalty: presPenalty,
			maxOutputTokens:      maxTokens,
			systemPromptPrefix:   providerCfg.SystemPromptPrefix,
			disableAutoSummarize: runtimeCfg.config.Options.DisableAutoSummarize,
			autoSummarizeAt:      runtimeCfg.config.Options.AutoSummarizeAt,
		}, nil
	})
}

// UpdateModels re-resolves the main agent's model from the current config
// and hands it, with its tools and system prompt, to agent. It is what the
// credential-refresh paths (auth.go) call to pick up fresh credentials,
// and what a config reload triggers.
func (b *runtimeBuilder) UpdateModels(ctx context.Context, agent SessionAgent, inputs runtimeToolInputs) error {
	runtime, err := b.runtimeFor(ctx, inputs)
	if errors.Is(err, errRuntimeChanged) {
		runtime, err = b.runtimeFor(ctx, inputs)
	}
	if err != nil {
		return err
	}
	agent.SetModel(runtime.model)
	agent.SetTools(runtime.tools)
	agent.SetSystemPrompt(runtime.systemPrompt)
	return nil
}

type runtimeOperationPort struct {
	agent  SessionAgent
	inputs runtimeToolInputs
}
