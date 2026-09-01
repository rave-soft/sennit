package agent

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	openaisdk "github.com/openai/openai-go/v3/option"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/discover"
	"github.com/rave-soft/sennit/internal/filetracker"
	historystore "github.com/rave-soft/sennit/internal/history/store"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/log"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/oauth/codex"
	"github.com/rave-soft/sennit/internal/oauth/copilot"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	"github.com/rave-soft/sennit/internal/pubsub"
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
	setMCPToken             func(*config.MCPTokenMutation, *oauth.Token) (bool, error)
	clearMCPToken           func(*config.MCPTokenMutation, *oauth.Token) (bool, error)
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

func (s runtimeConfigSnapshot) SetMCPToken(reservation *config.MCPTokenMutation, token *oauth.Token) (bool, error) {
	return s.setMCPToken(reservation, token)
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
	history                 historystore.Service
	filetracker             filetracker.Service
	background              *shell.BackgroundShellManager
	sessions                sessionstore.Service
	skillStates             []*skills.SkillState
}

type runtimeBuilder struct {
	cfg         *config.ConfigStore
	credentials *credentials.Manager
	notify      pubsub.Publisher[notify.Notification]
	mcp         *mcp.Registry
	interactive bool

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

	// accStoreOnce/accStore lazily construct the shared accounts.Store
	// used to list a provider's candidates for Pick. Lazy (rather than
	// set in the coordinator's struct literal) for the same reason as
	// rotators: several tests build a bare &runtimeBuilder{}, and a
	// production build gets exactly the same accounts.NewFileStore(...)
	// internal/workspace/app_workspace.go already constructs at its own
	// call sites, just constructed once here instead of per-call.
	accStoreOnce sync.Once
	accStore     accounts.Store
}

// accountStore returns the builder's shared accounts.Store, constructing
// it on first use. A test may pre-set b.accStore (e.g. to a fake) before
// this is ever called; production code never does, so it always resolves
// to accounts.NewFileStore(config.GlobalAccountsFile()).
func (b *runtimeBuilder) accountStore() accounts.Store {
	b.accStoreOnce.Do(func() {
		if b.accStore == nil {
			b.accStore = accounts.NewFileStore(config.GlobalAccountsFile())
		}
	})
	return b.accStore
}

// rotatorFor returns providerCfg's Rotator, building it on first use, or
// nil when rotation is disabled for this provider (no Rotation config, or
// Rotation.Enabled false).
//
// This is the single switch that makes rotation a complete no-op when
// disabled: every rotation call site (makeThresholdRotateCallback,
// makeRateLimitCallback) starts here and returns nil itself as soon as
// this does, so a disabled provider never gets a Rotator constructed, never
// consults accountStore, and never wires an OnRateLimit/RotateThreshold
// callback onto a call at all - behavior is provably identical to before
// rotation existed, not merely "happens to be a no-op" once invoked.
func (b *runtimeBuilder) rotatorFor(providerCfg config.ProviderConfig) *accounts.Rotator {
	if providerCfg.Rotation == nil || !providerCfg.Rotation.Enabled {
		return nil
	}
	b.rotatorsMu.Lock()
	defer b.rotatorsMu.Unlock()
	if r, ok := b.rotators[providerCfg.ID]; ok {
		return r
	}
	if b.rotators == nil {
		b.rotators = make(map[string]*accounts.Rotator)
	}
	r := accounts.NewRotator(providerCfg.Rotation.ToPolicy())
	b.rotators[providerCfg.ID] = r
	return r
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

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (b *runtimeBuilder) buildAgentModel(ctx context.Context, isSubAgent bool) (Model, error) {
	return b.buildAgentModelForConfig(ctx, b.cfg.Config(), isSubAgent)
}

func (b *runtimeBuilder) buildAgentModelForConfig(ctx context.Context, runtimeCfg *config.Config, isSubAgent bool) (Model, error) {
	return b.buildAgentModelForSnapshot(ctx, runtimeConfigSnapshot{config: runtimeCfg, resolver: runtimeCfg.RuntimeResolver(), workingDir: b.cfg.WorkingDir()}, isSubAgent)
}

func (b *runtimeBuilder) buildAgentModelForSnapshot(ctx context.Context, snapshot runtimeConfigSnapshot, isSubAgent bool) (Model, error) {
	runtimeCfg := snapshot.config
	modelCfg := runtimeCfg.Model
	if modelCfg.Model == "" {
		return Model{}, errModelNotSelected
	}

	providerCfg, ok := runtimeCfg.Providers.Get(modelCfg.Provider)
	if !ok {
		return Model{}, errModelProviderNotConfigured
	}

	return b.buildModelForSnapshot(ctx, providerCfg, modelCfg, isSubAgent, snapshot)
}

// buildCustomAgentModel builds the Model for an agent whose Model field
// names a specific model, e.g. "provider/model-id", rather than inheriting
// the app's main model. Config-load validation already guarantees that any
// such string reaching here resolves against the configured providers, but
// the config can be reloaded or edited after an agent is set up, so this
// still fails safe instead of trusting that blindly. ResolveModelString is
// reused rather than re-deriving its ambiguity resolution (matching a bare
// model ID against every provider, disambiguating a "provider/model" prefix
// from a model ID that itself contains a slash, etc.).
func (b *runtimeBuilder) buildCustomAgentModel(ctx context.Context, agent config.Agent, isSubAgent bool) (Model, error) {
	match, err := config.ResolveModelString(b.cfg.Config().Providers.Copy(), agent.Model)
	if err != nil {
		return Model{}, fmt.Errorf("agent %q model %q: %w", agent.Name, agent.Model, err)
	}

	providerCfg, ok := b.cfg.Config().Providers.Get(match.Provider)
	if !ok {
		return Model{}, fmt.Errorf("agent %q model %q: provider %q not configured", agent.Name, agent.Model, match.Provider)
	}

	selected := config.SelectedModel{Provider: match.Provider, Model: match.ModelID}

	model, err := b.buildModel(ctx, providerCfg, selected, isSubAgent)
	if errors.Is(err, errModelNotFound) {
		return Model{}, fmt.Errorf("agent %q model %q: model not found in provider config", agent.Name, agent.Model)
	}
	if err != nil {
		return Model{}, err
	}
	return model, nil
}

// buildModel resolves selected's provider, catalog entry, and language
// model, and assembles the result. Shared by buildAgentModel (the app's
// main model) and buildCustomAgentModel (an agent's own
// "provider/model-id"): both need the same provider-build ->
// catalog-lookup -> openrouter ":exacto" suffix -> language-model steps,
// and only differ in how they arrive at providerCfg/selected and in how
// they word a not-found error, which is why each keeps that part to
// itself and reports a bare errModelNotFound here for the caller to wrap.
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
		setMCPToken:             b.cfg.SetMCPToken,
		clearMCPToken:           b.cfg.ClearMCPToken,
	}
}

func (b *runtimeBuilder) runtimeFor(ctx context.Context, inputs runtimeToolInputs) (*compiledRuntime, error) {
	return b.runtime.getOrBuild(ctx, b.runtimeKey, func(ctx context.Context, key runtimeKey) (*compiledRuntime, error) {
		runtimeCfg := b.runtimeConfigSnapshot()
		model, err := b.buildAgentModelForSnapshot(ctx, runtimeCfg, false)
		if err != nil {
			return nil, err
		}
		agentCfg, ok := runtimeCfg.config.Agents[config.AgentCoder]
		if !ok {
			return nil, errCoderAgentNotConfigured
		}
		builtTools, err := b.buildToolsForConfig(ctx, agentCfg, false, inputs, runtimeCfg)
		if err != nil {
			return nil, err
		}
		runtimePrompt, err := coderPrompt(prompt.WithWorkingDir(runtimeCfg.workingDir))
		if err != nil {
			return nil, err
		}
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
		options, temp, topP, topK, freqPenalty, presPenalty := b.mergeCallOptions(model, providerCfg)
		maxTokens := modelMaxOutputTokens(model)
		return &compiledRuntime{
			key: key, model: model, tools: builtTools, systemPrompt: systemPrompt,
			providerCfg: providerCfg, providerOptions: options,
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

// --- credential refresh -------------------------------------------------

// refreshTokenIfExpired proactively refreshes the OAuth token if it has expired.
func (b *runtimeBuilder) refreshTokenIfExpired(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	if providerCfg.OAuthToken == nil || !providerCfg.OAuthToken.IsExpired() {
		return nil
	}
	slog.Debug("Token needs to be refreshed", "provider", providerCfg.ID)
	return b.refreshOAuth2Token(ctx, providerCfg, port)
}

// retryAfterUnauthorized attempts to refresh credentials after an auth error
// and returns nil if the request should be retried. For OAuth providers whose
// refresh token is revoked, and for Bedrock providers whose AWS SSO session
// has expired, it triggers interactive re-authentication and blocks until the
// user completes it (or the context is cancelled).
func (b *runtimeBuilder) retryAfterUnauthorized(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	switch {
	case providerCfg.OAuthToken != nil:
		slog.Debug("Received 401. Refreshing token and retrying", "provider", providerCfg.ID)
		if err := b.refreshOAuth2Token(ctx, providerCfg, port); err != nil {
			// If the refresh token was revoked, trigger interactive
			// re-auth and wait for the user to complete it.
			var exchangeErr *oauth.TokenExchangeError
			if b.notify != nil && errors.As(err, &exchangeErr) && exchangeErr.IsRefreshTokenRevoked() {
				slog.Info("Refresh token revoked, waiting for re-authentication", "provider", providerCfg.ID)
				b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeReAuthenticate,
					ProviderID: providerCfg.ID,
				})
				return b.waitForInteractiveReauth(ctx, providerCfg.ID, port)
			}
			return err
		}
		return nil
	case providerCfg.AWSAuthRefresh != "":
		return b.refreshAWSCredentials(ctx, providerCfg, port)
	case strings.Contains(providerCfg.APIKeyTemplate, "$"):
		slog.Debug("Received 401. Refreshing API Key template and retrying", "provider", providerCfg.ID)
		return b.refreshApiKeyTemplate(ctx, providerCfg, port)
	default:
		return nil
	}
}

// errNoInteractiveAuth is returned by an OnAuthRefresh callback when a
// provider needs interactive re-authentication but no notifier is available
// to drive it (e.g. headless runs). Returning it surfaces the original auth
// error rather than retrying.
var errNoInteractiveAuth = errors.New("interactive authentication unavailable")

// waitForInteractiveReauth blocks until interactive re-authentication for the
// provider completes (signalled via SignalAuthComplete) or the context is
// cancelled, then rebuilds models so the next attempt picks up fresh
// credentials. Returns nil when the caller should retry.
func (b *runtimeBuilder) waitForInteractiveReauth(ctx context.Context, providerID string, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	// Use a detached context with a generous timeout so the wait survives
	// agent run cancellation. The user needs time to complete browser-based
	// authentication.
	waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer waitCancel()
	slog.Info("Blocking on WaitForTokenChange", "provider", providerID)
	if waitErr := b.credentials.WaitForTokenChange(waitCtx, providerID); waitErr != nil {
		slog.Info("WaitForTokenChange returned error", "provider", providerID, "error", waitErr)
		return waitErr
	}
	// If the original context was cancelled during the wait, fantasy's retry
	// would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		slog.Warn("Original context cancelled during auth wait, cannot retry",
			"provider", providerID, "ctx_err", ctx.Err())
		return ctx.Err()
	}
	// Rebuild models so ModelProvider picks up the fresh credentials.
	if agent == nil {
		return nil
	}
	if updateErr := b.UpdateModels(waitCtx, agent, inputs); updateErr != nil {
		slog.Error("Failed to update models after re-authentication", "error", updateErr)
		return updateErr
	}
	slog.Info("Models updated, returning nil to retry", "provider", providerID)
	return nil
}

// buildProviderHTTPClient returns an OnAuthRefresh callback for fantasy that
// delegates to the builder's existing credential refresh logic. Returns
// nil if no refresh mechanism is configured for the provider. If active is
// non-nil, it is refreshed with the recompiled runtime after a successful
// credential refresh; pass nil when there is no active runtime to track.
func (b *runtimeBuilder) makeAuthRefreshCallback(providerCfg config.ProviderConfig, active *activeRuntime, port runtimeOperationPort) func(context.Context, *fantasy.ProviderError) error {
	inputs := port.inputs
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		if err := b.retryAfterUnauthorized(ctx, providerCfg, port); err != nil {
			return err
		}
		if active != nil {
			runtime, err := b.runtimeFor(ctx, inputs)
			if err != nil {
				return err
			}
			active.store(runtime)
		}
		return nil
	}
}

// makeSubAgentAuthRefreshCallback is makeAuthRefreshCallback's counterpart
// for a delegation whose model differs from the coordinator's own (a
// custom agent's own "provider/model-id", or merely a cheaper model on the
// same OAuth provider). makeAuthRefreshCallback stores b.runtimeFor's
// result into active after a refresh, but that always rebuilds the
// coder/top-level runtime; modelProvider (turn.go) only adopts what is in
// active when both the provider AND the model match the turn's own, so for
// any sub-agent running a different model that store was silently a
// no-op - the retry kept using the pre-refresh provider instance built
// from t.model's stale credential, hit 401 again, and fantasy only
// refreshes once per pass, so the delegation died on an expiry the
// top-level agent recovers from cleanly. This variant rebuilds a runtime
// scoped to model (the sub-agent's own, already resolved by buildAgentModel
// or buildCustomAgentModel in buildAgent) instead, so what lands in active
// actually matches what modelProvider is comparing against.
func (b *runtimeBuilder) makeSubAgentAuthRefreshCallback(providerCfg config.ProviderConfig, model Model, active *activeRuntime, port runtimeOperationPort) func(context.Context, *fantasy.ProviderError) error {
	if providerCfg.OAuthToken == nil &&
		!strings.Contains(providerCfg.APIKeyTemplate, "$") &&
		providerCfg.AWSAuthRefresh == "" {
		return nil
	}
	return func(ctx context.Context, _ *fantasy.ProviderError) error {
		if err := b.retryAfterUnauthorized(ctx, providerCfg, port); err != nil {
			return err
		}
		if active != nil {
			runtime, err := b.buildSubAgentRuntime(ctx, model)
			if err != nil {
				return err
			}
			active.store(runtime)
		}
		return nil
	}
}

// buildSubAgentRuntime rebuilds model's provider/language-model pair against
// the current config (i.e. after a credential refresh has landed a new
// token or key), the same way buildAgentModel/buildCustomAgentModel do for
// a fresh delegation build. It re-reads providerCfg from the live config
// store rather than trusting a value captured before the refresh, since
// that value's credential is exactly what the refresh just replaced.
// Deliberately minimal: the only field a sub-agent's OnAuthRefresh path
// reads back out of the stored compiledRuntime is .model (see
// modelProvider in turn.go and summarize's ModelProvider in usage.go) -
// tools and system prompt are not rebuilt here, unlike runtimeFor's
// coder-runtime construction, because nothing on this path consults them.
func (b *runtimeBuilder) buildSubAgentRuntime(ctx context.Context, model Model) (*compiledRuntime, error) {
	providerCfg, ok := b.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}
	rebuilt, err := b.buildModel(ctx, providerCfg, model.ModelCfg, true)
	if err != nil {
		return nil, err
	}
	return &compiledRuntime{model: rebuilt, providerCfg: providerCfg}, nil
}

// currentRotationAccount resolves the account a rotation callback should
// act on for providerCfg's provider. providerCfg is captured by value once
// per turn (turn_dispatcher.go), so after the first rotation it still names
// the pre-rotation account; active, if present, is restored on every
// successful applyRotationPick with a freshly rebuilt runtime whose
// providerCfg carries the account that is actually live now. Falling back
// to providerCfg.Account keeps this correct for callers that pass a nil
// active (e.g. no top-level agent to rebuild for).
//
// active is shared with makeAuthRefreshCallback, which stores whatever
// runtimeFor built for the CURRENT config - not necessarily this provider.
// If the user switches the main model to a different provider mid-turn and
// a 401 refresh runs on it while this provider is still streaming (a
// sub-agent on a second provider, say), active now describes that other
// provider. Trusting its Account blindly would mark/rotate an account this
// provider's Rotator has never heard of. Only adopt the loaded runtime's
// account when it was actually built for providerCfg; otherwise fall back
// to the captured value exactly as when active is nil.
func currentRotationAccount(providerCfg config.ProviderConfig, active *activeRuntime) string {
	if active != nil {
		if runtime := active.load(); runtime != nil && runtime.providerCfg.ID == providerCfg.ID {
			return runtime.providerCfg.Account
		}
	}
	return providerCfg.Account
}

// accountLabel returns a's display name for a rotation notification:
// its user-editable Label when set, its bookkeeping ID otherwise.
func accountLabel(a accounts.Account) string {
	if a.Label != "" {
		return a.Label
	}
	return a.ID
}

// worstKnownRemainingPercent returns 100 minus the highest UsedPercent
// among a's known usage windows - the remaining allowance on whichever
// window is closest to exhausted, which is the one that actually tripped
// ShouldRotate. Returns -1 when neither window is known (nothing to
// report; callers omit the percent from their message in that case).
func worstKnownRemainingPercent(u accounts.Usage) int {
	worst := -1
	for _, w := range []accounts.UsageWindow{u.Primary, u.Secondary} {
		if w.Known() && w.UsedPercent > worst {
			worst = w.UsedPercent
		}
	}
	if worst < 0 {
		return -1
	}
	return 100 - worst
}

// applyRotationPick activates picked as providerID's active account and,
// when active is non-nil, rebuilds and stores the runtime so the next
// request actually uses the new credentials - the same two steps
// makeAuthRefreshCallback takes after a successful credential refresh
// (plan §5.2: activation is projected into the live ProviderConfig, never
// touching the provider build path itself).
func (b *runtimeBuilder) applyRotationPick(ctx context.Context, providerID string, picked accounts.Account, active *activeRuntime, inputs runtimeToolInputs) error {
	if err := b.cfg.ActivateAccount(config.ScopeGlobal, providerID, picked); err != nil {
		return fmt.Errorf("activating rotated account %s for provider %s: %w", picked.ID, providerID, err)
	}
	if active == nil {
		return nil
	}
	runtime, err := b.runtimeFor(ctx, inputs)
	if err != nil {
		return fmt.Errorf("rebuilding runtime after rotating provider %s to account %s: %w", providerID, picked.ID, err)
	}
	active.store(runtime)
	return nil
}

// makeThresholdRotateCallback returns the RotateThreshold hook (plan
// §5.5's proactive trigger, Codex today): called once per finished step,
// it checks the active account's last usage snapshot and, if
// accounts.Rotator.ShouldRotate says the account is over threshold,
// switches to the next usable one.
//
// Returns nil - meaning "nothing to do here, ever" - when rotation is
// disabled for providerCfg (rotatorFor's nil check) or the provider isn't
// a RotateThreshold one, so a RotateRateLimit or RotateNever provider
// never even gets this hook wired onto a call.
//
// The returned function never fails the turn: every error path logs and
// returns, exactly matching what happens today when a request simply runs
// over quota on a single-account setup - the user keeps using the current
// (over-threshold) account rather than losing the step's own result over
// a rotation that didn't work out.
func (b *runtimeBuilder) makeThresholdRotateCallback(providerCfg config.ProviderConfig, active *activeRuntime, port runtimeOperationPort) func(context.Context) {
	rotator := b.rotatorFor(providerCfg)
	if rotator == nil || accounts.CapabilitiesOf(providerCfg.ID).RotateOn != accounts.RotateThreshold {
		return nil
	}
	inputs := port.inputs
	return func(ctx context.Context) {
		// Resolve the account live rather than trusting providerCfg.Account:
		// providerCfg is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment).
		account := currentRotationAccount(providerCfg, active)
		// RotateThreshold is Codex-only today (see capabilities.go), so
		// reading its usage snapshot straight from the codex package is
		// deliberate, not a layering slip - a future non-Codex threshold
		// provider would need this coupling broken out (e.g. a small
		// per-provider usage-lookup registry) before it could reuse this
		// path.
		usage, ok := codex.UsageFor(account)
		if !ok {
			return
		}
		all, err := b.accountStore().List(providerCfg.ID)
		if err != nil {
			slog.Warn("Threshold rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			return
		}
		acct := accounts.Account{ID: account, Usage: usage.Snapshot()}
		for i, a := range all {
			if a.ID == account {
				acct = a
				acct.Usage = usage.Snapshot()
				// Pick reads exhaustion off its own candidates list, not
				// off acct separately (unlike ShouldRotate, which
				// takes acct directly) - without this, Pick would see
				// the store's possibly-stale Usage for the active
				// account and, finding it "unknown" rather than
				// exhausted, could pick the very account this callback
				// is trying to rotate away from.
				all[i] = acct
				break
			}
		}
		if !rotator.ShouldRotate(acct, all) {
			return
		}
		picked, err := rotator.Pick(providerCfg.ID, acct.ID, all)
		if err != nil {
			slog.Warn("Threshold rotation: no usable account", "provider", providerCfg.ID, "error", err)
			return
		}
		if picked.ID == acct.ID {
			return
		}
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, inputs); err != nil {
			slog.Warn("Threshold rotation: failed to apply picked account", "provider", providerCfg.ID, "error", err)
			return
		}
		if b.notify != nil {
			remaining := worstKnownRemainingPercent(acct.Usage)
			msg := fmt.Sprintf("%s: switched to %q", providerCfg.Name, accountLabel(picked))
			if remaining >= 0 {
				msg = fmt.Sprintf("%s, %q had %d%% left", msg, accountLabel(acct), remaining)
			}
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:       notify.TypeAccountRotated,
				ProviderID: providerCfg.ID,
				Message:    msg,
			})
		}
	}
}

// makeRateLimitCallback returns the fantasy OnRateLimitFunc for the
// reactive rotation trigger (plan §5.5, every RotateRateLimit provider):
// on a 429, it marks the active account cooling down, picks the next
// usable one via the provider's Rotator, and applies it exactly like
// makeThresholdRotateCallback (§5.2 projection + runtime rebuild).
//
// Returns nil when rotation is disabled for providerCfg or the provider
// isn't a RotateRateLimit one, mirroring makeAuthRefreshCallback's own
// "no mechanism configured" nil return - fantasy never engages an unset
// hook, so a disabled/non-matching provider's retry behavior is untouched.
//
// On success, the returned function returns nil so fantasy retries
// immediately with the new account's credentials (RetryOptions.OnRateLimit's
// contract). When every candidate is exhausted, it returns the
// *accounts.ErrAllExhausted from Pick unchanged, which RetryOptions.OnRateLimit
// treats as "rotation didn't help" - normal backoff resumes and the
// ORIGINAL 429 (not this error) is what a caller ultimately sees; see
// RetryWithExponentialBackoffRespectingRetryHeaders and runTurn.handleStreamError.
func (b *runtimeBuilder) makeRateLimitCallback(providerCfg config.ProviderConfig, active *activeRuntime, port runtimeOperationPort) fantasy.OnRateLimitFunc {
	rotator := b.rotatorFor(providerCfg)
	if rotator == nil || accounts.CapabilitiesOf(providerCfg.ID).RotateOn != accounts.RotateRateLimit {
		return nil
	}
	inputs := port.inputs
	return func(ctx context.Context, providerErr *fantasy.ProviderError) error {
		// Resolve the account live rather than trusting providerCfg.Account:
		// providerCfg is captured by value once per turn, so after a
		// rotation it still names the pre-rotation account (see
		// currentRotationAccount's doc comment) - without this, a second
		// 429 on the newly-picked account would mark the WRONG account
		// rate-limited and hot-loop retrying on the still-limited one.
		account := currentRotationAccount(providerCfg, active)
		rotator.MarkRateLimited(account, retryAfterFromHeaders(providerErr))

		all, err := b.accountStore().List(providerCfg.ID)
		if err != nil {
			slog.Warn("Rate-limit rotation: failed to list accounts", "provider", providerCfg.ID, "error", err)
			return err
		}
		picked, err := rotator.Pick(providerCfg.ID, account, all)
		if err != nil {
			var exhausted *accounts.ErrAllExhausted
			if errors.As(err, &exhausted) && b.notify != nil {
				msg := fmt.Sprintf("%s: all accounts exhausted", providerCfg.Name)
				if !exhausted.ResetsAt.IsZero() {
					msg = fmt.Sprintf("%s, resets at %s", msg, exhausted.ResetsAt.Format("15:04"))
				}
				b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
					Type:       notify.TypeAccountRotationExhausted,
					ProviderID: providerCfg.ID,
					Message:    msg,
				})
			}
			return err
		}
		if picked.ID == account {
			// Pick found nothing better to switch to - most commonly a
			// single-account provider, where Pick's one-candidate fast
			// path hands back the very account MarkRateLimited just put
			// on cooldown without even consulting it. Applying picked
			// would be a no-op ActivateAccount call for no reason. But
			// returning nil here would tell fantasy "credentials
			// rotated, retry immediately" (RetryOptions.OnRateLimit),
			// which fires the very next attempt at the still-limited
			// account with no delay at all, burning a retry for
			// nothing. Return ErrAllExhausted instead - Pick's own
			// verdict for "no usable account right now" - so
			// OnRateLimit's error path takes over and normal backoff
			// applies before the retry.
			return &accounts.ErrAllExhausted{ProviderID: providerCfg.ID}
		}
		if err := b.applyRotationPick(ctx, providerCfg.ID, picked, active, inputs); err != nil {
			slog.Warn("Rate-limit rotation: failed to apply picked account", "provider", providerCfg.ID, "error", err)
			return err
		}
		if b.notify != nil {
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:       notify.TypeAccountRotated,
				ProviderID: providerCfg.ID,
				Message:    fmt.Sprintf("%s: switched to %q after a rate limit", providerCfg.Name, accountLabel(picked)),
			})
		}
		return nil
	}
}

// retryAfterFromHeaders extracts the Retry-After delay from a
// *fantasy.ProviderError's response headers, for MarkRateLimited. This
// deliberately duplicates the couple of lines third_party/fantasy/retry.go's
// unexported getRetryDelayInMs already does (retry-after-ms, then
// Retry-After as seconds or an HTTP date) rather than exporting that
// helper across the vendor boundary for one small caller - see plan §9
// risk 6 on keeping the fork's surface area minimal.
func retryAfterFromHeaders(err *fantasy.ProviderError) time.Duration {
	if err == nil || err.ResponseHeaders == nil {
		return 0
	}
	h := err.ResponseHeaders
	if ms, ok := h["retry-after-ms"]; ok {
		if v, parseErr := strconv.ParseFloat(ms, 64); parseErr == nil {
			return time.Duration(v) * time.Millisecond
		}
	}
	if ra, ok := h["retry-after"]; ok {
		if secs, parseErr := strconv.ParseFloat(ra, 64); parseErr == nil {
			return time.Duration(secs) * time.Second
		}
		if t, parseErr := time.Parse(time.RFC1123, ra); parseErr == nil {
			return time.Until(t)
		}
	}
	return 0
}

func (b *runtimeBuilder) refreshOAuth2Token(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	if err := b.credentials.RefreshOAuthToken(ctx, config.ScopeGlobal, providerCfg.ID); err != nil {
		slog.Error("Failed to refresh OAuth token after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}
	if agent == nil {
		return nil
	}
	return b.UpdateModels(ctx, agent, inputs)
}

func (b *runtimeBuilder) refreshApiKeyTemplate(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	newAPIKey, err := b.cfg.Resolve(providerCfg.APIKeyTemplate)
	if err != nil {
		slog.Error("Failed to re-resolve API key after 401 error", "provider", providerCfg.ID, "error", err)
		return err
	}

	if err := b.cfg.UpdateProviderCredentials(providerCfg.ID, newAPIKey, providerCfg.OAuthToken); err != nil {
		return err
	}

	if agent == nil {
		return nil
	}
	return b.UpdateModels(ctx, agent, inputs)
}

// refreshAWSCredentials runs the provider's configured AWS SSO refresh
// command (e.g. "aws sso login") on the machine that makes the Bedrock
// calls, streaming the verification URL to the UI for display, then rebuilds
// models so the AWS SDK re-reads the refreshed credentials. It returns nil to
// signal that the failed request should be retried.
//
// The command runs here, in the coordinator, rather than in the UI dialog so
// the refreshed credentials land where the model calls are made.
func (b *runtimeBuilder) refreshAWSCredentials(ctx context.Context, providerCfg config.ProviderConfig, port runtimeOperationPort) error {
	agent, inputs := port.agent, port.inputs
	if b.notify == nil {
		return errNoInteractiveAuth
	}
	slog.Info("AWS credentials expired, running refresh command",
		"provider", providerCfg.ID, "command", providerCfg.AWSAuthRefresh)

	// Open the dialog immediately so the user sees progress even before the
	// command prints its verification URL.
	b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
		Type:         notify.TypeAWSSSOAuth,
		ProviderID:   providerCfg.ID,
		AWSSOCommand: providerCfg.AWSAuthRefresh,
	})

	// Detach from the turn's context (with a generous timeout) so cancelling
	// the turn doesn't kill an in-progress browser login.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), awsSSORefreshTimeout)
	defer cancel()

	runErr := b.runAWSAuthRefresh(runCtx, providerCfg)

	result := notify.Notification{Type: notify.TypeAWSSSOAuthResult, ProviderID: providerCfg.ID}
	if runErr != nil {
		result.Message = runErr.Error()
	}
	b.notify.Publish(pubsub.CreatedEvent, result)

	if runErr != nil {
		slog.Error("AWS SSO refresh command failed", "provider", providerCfg.ID, "error", runErr)
		return runErr
	}
	// If the turn's context was cancelled while the command ran, fantasy's
	// retry would fail immediately, so surface the cancellation instead.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Rebuild models so the AWS SDK credential chain re-reads the refreshed
	// SSO cache on the next attempt.
	b.invalidateRuntime(runCtx, "aws_auth_refresh", func() bool { return true })
	if agent == nil {
		slog.Info("AWS SSO refresh complete, no top-level agent to update", "provider", providerCfg.ID)
		return nil
	}
	if err := b.UpdateModels(runCtx, agent, inputs); err != nil {
		slog.Error("Failed to update models after AWS SSO refresh", "provider", providerCfg.ID, "error", err)
		return err
	}
	slog.Info("AWS SSO refresh complete, retrying request", "provider", providerCfg.ID)
	return nil
}

// runAWSAuthRefresh executes the refresh command, publishing the SSO
// verification URL to the UI as soon as it appears in the output and
// returning any failure with captured stderr for context.
func (b *runtimeBuilder) runAWSAuthRefresh(ctx context.Context, providerCfg config.ProviderConfig) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", providerCfg.AWSAuthRefresh)
	cmd.Dir = b.cfg.WorkingDir()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	// Drain stdout and stderr concurrently so a command that fills one pipe
	// buffer before closing the other can't deadlock. Both are scanned for
	// the verification URL; stderr is also captured for error detail.
	var (
		stderrBuf bytes.Buffer
		mu        sync.Mutex // Guards the single-shot URL publish across goroutines.
		urlSent   bool
	)
	publishURL := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if urlSent {
			return
		}
		if url := extractAWSSSOURL(line); url != "" {
			urlSent = true
			// Second phase of the two-part publish: the dialog is already
			// open from refreshAWSCredentials; this fills in the URL on it.
			b.notify.Publish(pubsub.CreatedEvent, notify.Notification{
				Type:         notify.TypeAWSSSOAuth,
				ProviderID:   providerCfg.ID,
				AWSSOCommand: providerCfg.AWSAuthRefresh,
				AWSSOURL:     url,
			})
		}
	}

	var wg sync.WaitGroup
	var scanErrs [2]error
	scan := func(index int, name string, r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(nil, awsSSOOutputLineLimit)
		for scanner.Scan() {
			publishURL(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			scanErrs[index] = fmt.Errorf("read AWS auth refresh %s: %w", name, err)
			_, _ = io.Copy(io.Discard, r)
		}
	}
	wg.Add(2)
	go scan(0, "stdout", stdout)
	go scan(1, "stderr", io.TeeReader(stderrPipe, &stderrBuf))
	wg.Wait()

	waitErr := cmd.Wait()
	var stderrErr error
	if stderr := strings.TrimSpace(stderrBuf.String()); stderr != "" && waitErr != nil {
		stderrErr = fmt.Errorf("%w: %s", waitErr, stderr)
		waitErr = nil
	}
	var scanErr error
	for _, err := range scanErrs {
		scanErr = errors.Join(scanErr, err)
	}
	return errors.Join(waitErr, stderrErr, scanErr)
}

// --- provider construction ----------------------------------------------

// Copilot models that use the Responses API instead of Chat Completions.

// buildProviderHTTPClient returns an *http.Client composing proxy routing
// (proxyURL) with debug request logging, or (nil, nil) if neither applies —
// callers should skip WithHTTPClient in that case and use the SDK's default
// client. Proxying and debug logging compose: when both are set, requests
// go through the proxy and are logged.
func (b *runtimeBuilder) buildProviderHTTPClient(proxyURL string) (*http.Client, error) {
	return buildProviderHTTPClient(proxyURL, b.cfg.Config().Options.Debug)
}

func buildProviderHTTPClient(proxyURL string, debug bool) (*http.Client, error) {
	if proxyURL == "" && !debug {
		return nil, nil
	}
	transport, err := buildProxyTransport(proxyURL)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	if debug {
		transport = &log.HTTPRoundTripLogger{Transport: transport}
	}
	return &http.Client{Transport: transport}, nil
}

func (b *runtimeBuilder) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
	var opts []anthropic.Option
	authIsBearer := false

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		headers["Authorization"] = apiKey
		authIsBearer = true
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		headers["Authorization"] = "Bearer " + apiKey
		authIsBearer = true
	case apiKey != "":
		// X-Api-Key header
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
	if err != nil {
		return nil, err
	}
	if authIsBearer {
		// Auth goes through Authorization above, so we never pass
		// anthropic.WithAPIKey — which means the SDK's own
		// DefaultClientOptions falls back to reading $ANTHROPIC_API_KEY and
		// setting X-Api-Key from it, duplicating (or contradicting) the
		// Bearer token. option.WithAPIKey("") is not a fix: WithHeader uses
		// Header.Set, so it would send an empty X-Api-Key rather than omit
		// it. This used to be worked around with
		// os.Setenv("ANTHROPIC_API_KEY", ""), which corrupted the key for
		// every other provider built afterwards and every subprocess
		// Sennit spawns. Stripping the header at the transport, the same
		// seam azureAPIVersionTransport uses in providers.go, is local and
		// leaves the environment untouched.
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = &stripHeaderTransport{
			base:   httpClient.Transport,
			header: "X-Api-Key",
		}
	}
	if httpClient != nil {
		opts = append(opts, anthropic.WithHTTPClient(httpClient))
	}
	return anthropic.New(opts...)
}

// stripHeaderTransport deletes a header the SDK set from its own defaults
// (see buildAnthropicProvider) before the request goes out, without
// touching the process environment those defaults were read from.
type stripHeaderTransport struct {
	base   http.RoundTripper
	header string
}

func (t *stripHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Header.Get(t.header) != "" {
		req = req.Clone(req.Context())
		req.Header.Del(t.header)
	}
	return base.RoundTrip(req)
}

func (b *runtimeBuilder) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
	if err != nil {
		return nil, err
	}
	if providerID == codex.ProviderID {
		// Codex quotes the account's plan and remaining allowance on every
		// response, so the sidebar's figures come from ordinary traffic
		// rather than a separate poll — but only if something reads the
		// headers, which is what this transport is for.
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = codex.NewUsageTransport(httpClient.Transport)
	}
	if httpClient != nil {
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (b *runtimeBuilder) buildOpenrouterProvider(_, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (b *runtimeBuilder) buildVercelProvider(_, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

func (b *runtimeBuilder) buildOpenaiCompatProvider(baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode.
	var httpClient *http.Client
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		proxyTransport, err := buildProxyTransport(proxyURL)
		if err != nil {
			return nil, err
		}
		httpClient = copilot.NewClient(isSubAgent, debug, proxyTransport)
	}
	if httpClient == nil {
		var err error
		httpClient, err = buildProviderHTTPClient(proxyURL, debug)
		if err != nil {
			return nil, err
		}
	}
	if httpClient != nil {
		opts = append(opts, openaicompat.WithHTTPClient(httpClient))
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (b *runtimeBuilder) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	httpClient, err := buildProviderHTTPClient(proxyURL, debug)
	if err != nil {
		return nil, err
	}
	if options == nil {
		options = make(map[string]string)
	}
	// fantasy's azure provider (charm.land/fantasy/providers/azure) stores
	// WithAPIVersion but never reads it back out (confirmed against our
	// pinned v0.40.0 and the newest published v0.41.2 alike) — azure.New
	// never applies it to the request, so passing it straight through
	// would silently do nothing. fantasy does let us supply the HTTP
	// client every Azure request goes through (WithHTTPClient, same seam
	// codex.NewUsageTransport above uses), so we honour the setting
	// ourselves by adding the api-version query parameter at the
	// transport level instead of dropping it.
	if apiVersion := options["apiVersion"]; apiVersion != "" {
		if httpClient == nil {
			httpClient = &http.Client{}
		}
		httpClient.Transport = &azureAPIVersionTransport{
			base:       httpClient.Transport,
			apiVersion: apiVersion,
		}
	}
	if httpClient != nil {
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

// azureAPIVersionTransport adds Azure's required "api-version" query
// parameter to every outgoing request, since fantasy's azure provider
// accepts the option but never applies it (see buildAzureProvider). It
// leaves an already-present api-version alone, in case a future fantasy
// release starts setting one itself.
type azureAPIVersionTransport struct {
	base       http.RoundTripper
	apiVersion string
}

func (t *azureAPIVersionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.URL != nil && req.URL.Query().Get("api-version") == "" {
		req = req.Clone(req.Context())
		q := req.URL.Query()
		q.Set("api-version", t.apiVersion)
		req.URL.RawQuery = q.Encode()
	}
	return base.RoundTrip(req)
}

func (b *runtimeBuilder) buildBedrockProvider(apiKey string, headers map[string]string, providerID, proxyURL string, debug bool) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}

	usingAPIKeyAuth := false
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
		usingAPIKeyAuth = true
	case os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
		usingAPIKeyAuth = true
	default:
		// Skip, let the SDK do authentication.
	}

	switch providerID {
	case string(catwalk.InferenceProviderBedrockEurope):
		// The EU variant always forces its region: it exists specifically
		// to pin requests to eu-west-1, so there is no "let the user's own
		// region win" case here.
		opts = append(opts, bedrock.WithRegion("eu-west-1"))
	default:
		// Only force us-east-1 as a last resort, and only where it can
		// actually change anything. fantasy's anthropic provider
		// (anthropic.go:289-296) consults the region we pass here in two
		// different ways depending on auth: with an API key or skipAuth it
		// builds config via bedrockBasicAuthConfig, which already does
		// cmp.Or(region, "us-east-1") and never looks at the environment -
		// so our forcing it here changes nothing and the check below would
		// be wasted work. Only the SDK-auth path (no API key) does
		// cfg.Region = cmp.Or(bedrockRegion, cfg.Region) against a
		// LoadDefaultConfig result, where any region we pass always wins
		// over one the environment already supplies.
		//
		// We don't call LoadDefaultConfig ourselves to check: on a
		// non-EC2 box with no AWS config, region resolution can fall
		// through to the EC2 instance-metadata endpoint and stall a
		// provider build (this runs on every config reload and model
		// switch, not just once). AWS_REGION/AWS_DEFAULT_REGION is a
		// cheaper, synchronous proxy for "the environment supplies a
		// region" - it won't see a region set only in a shared AWS config
		// profile, so a profile-only region still gets overridden to
		// us-east-1, but it covers the common case without risking a
		// network stall in the UI's path.
		if !usingAPIKeyAuth && os.Getenv("AWS_REGION") == "" && os.Getenv("AWS_DEFAULT_REGION") == "" {
			opts = append(opts, bedrock.WithRegion("us-east-1"))
		}
	}

	return bedrock.New(opts...)
}

func (b *runtimeBuilder) buildGoogleProvider(baseURL, apiKey string, headers map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (b *runtimeBuilder) buildGoogleVertexProvider(headers map[string]string, options map[string]string, proxyURL string, debug bool) (fantasy.Provider, error) {
	opts := []google.Option{}
	if httpClient, err := buildProviderHTTPClient(proxyURL, debug); err != nil {
		return nil, err
	} else if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (b *runtimeBuilder) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

// buildProvider returns a fantasy.Provider for the configured provider,
// resolving its API key and base URL through the config's shell-expansion
// resolver.
func (b *runtimeBuilder) buildProvider(providerCfg config.ProviderConfig, model config.SelectedModel) (fantasy.Provider, error) {
	return b.buildProviderForSnapshot(providerCfg, model, false, b.runtimeConfigSnapshot())
}

func (b *runtimeBuilder) buildProviderForSnapshot(providerCfg config.ProviderConfig, model config.SelectedModel, isSubAgent bool, snapshot runtimeConfigSnapshot) (fantasy.Provider, error) {
	headers := maps.Clone(providerCfg.ExtraHeaders)
	if headers == nil {
		headers = make(map[string]string)
	}

	// handle special headers for anthropic
	if providerCfg.Type == anthropic.Name && b.isAnthropicThinking(model) {
		if v, ok := headers["anthropic-beta"]; ok {
			headers["anthropic-beta"] = v + ",interleaved-thinking-2025-05-14"
		} else {
			headers["anthropic-beta"] = "interleaved-thinking-2025-05-14"
		}
	}

	// A resolve failure (an env var that is not set, a shell command that
	// exits non-zero) leaves the value empty, and the provider then fails
	// on an empty key or URL with nothing pointing at the cause. Log it
	// here rather than dropping it: the call still proceeds on what it
	// has, which is what it did before, but the reason is now on the
	// record.
	apiKey, err := snapshot.resolver.ResolveValue(providerCfg.APIKey)
	if err != nil {
		slog.Warn("Failed to resolve provider API key", "provider", providerCfg.ID, "error", err)
	}
	baseURL, err := snapshot.resolver.ResolveValue(providerCfg.BaseURL)
	if err != nil {
		slog.Warn("Failed to resolve provider base URL", "provider", providerCfg.ID, "error", err)
	}

	switch providerCfg.ID {
	case string(catwalk.InferenceProviderOpenCodeGo), string(catwalk.InferenceProviderOpenCodeZen):
		if opencodeMessagesModels[model.Model] {
			baseURL = strings.TrimSuffix(baseURL, "/v1")
			return b.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL, snapshot.config.Options.Debug)
		}
	}

	switch providerCfg.Type {
	case openai.Name:
		return b.buildOpenaiProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case anthropic.Name:
		return b.buildAnthropicProvider(baseURL, apiKey, headers, providerCfg.ID, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case openrouter.Name:
		return b.buildOpenrouterProvider(baseURL, apiKey, headers, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case vercel.Name:
		return b.buildVercelProvider(baseURL, apiKey, headers, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case azure.Name:
		return b.buildAzureProvider(baseURL, apiKey, headers, providerCfg.ExtraParams, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case bedrock.Name:
		return b.buildBedrockProvider(apiKey, headers, providerCfg.ID, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case google.Name:
		return b.buildGoogleProvider(baseURL, apiKey, headers, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case "google-vertex":
		return b.buildGoogleVertexProvider(headers, providerCfg.ExtraParams, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	case openaicompat.Name:
		switch providerCfg.ID {
		case string(catwalk.InferenceProviderZAI):
			// Clone before writing: providerCfg.ExtraBody is shared with
			// the stored *config.Config, and mutating it in place would
			// race other readers and leak the flag into later generations.
			extraBody := maps.Clone(providerCfg.ExtraBody)
			if extraBody == nil {
				extraBody = map[string]any{}
			}
			extraBody["tool_stream"] = true
			return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, extraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL, snapshot.config.Options.Debug)
		}
		return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL, snapshot.config.Options.Debug)
	default:
		// Known custom providers (litellm, ollama, omlx) are
		// openai-compat under the hood.
		if discover.IsKnownCustomProvider(string(providerCfg.Type)) {
			return b.buildOpenaiCompatProvider(baseURL, apiKey, headers, providerCfg.ExtraBody, providerCfg.ID, isSubAgent, providerCfg.ProxyURL, snapshot.config.Options.Debug)
		}
		return nil, fmt.Errorf("provider type not supported: %q", providerCfg.Type)
	}
}
