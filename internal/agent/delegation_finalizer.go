package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
)

// delegationFinalizer owns everything that happens *around* a delegation:
// the thread/task adapter generations the delegation tools are built from,
// the sub-session busy/cost bookkeeping, the launch and run paths of the
// agent/agentic_fetch/custom-agent tools, and the skill snapshot the tool
// set is compiled from. Delegation *delivery* (completion inbox,
// continuation wake, SendToParent) stays on the sessionAgent's dispatcher:
// it is per-session turn state, not per-coordinator state.
type delegationFinalizer struct {
	*agentDeps

	builder *runtimeBuilder

	lifecycle *readinessLifecycle

	// fetch lazily builds the shared *http.Client the agentic-fetch tool
	// falls back to when no caller-supplied client is given.
	fetch fetchClient

	// subSessions counts the delegations currently running under each
	// sub-agent session id, so IsSessionBusy can answer for them.
	//
	// A sub-agent runs on its own SessionAgent, and every SessionAgent
	// carries its own dispatcher — so the child session's dispatch state
	// lives in the delegate, not in currentAgent. Asking currentAgent
	// alone therefore reports a mid-flight delegation as idle, and the
	// UI asks exactly that when it loads a session: navigating into a
	// sub-agent while it is working drew a spinner that never ticked,
	// because the animation is only armed for a session the agent says
	// is busy.
	subSessionsMu sync.Mutex
	subSessions   map[string]int

	// delegationTools holds the thread/task adapters as one immutable
	// generation. Attach can publish a new generation while runs build or
	// invoke tools, but a reader always receives a complete pair.
	delegationTools atomic.Pointer[delegationToolsSnapshot]

	// skills holds the coordinator's skill discovery snapshot.
	skills skillsState

	agentPort *coordinatorAgentPort

	// runtimeInputsMu guards runtimeInputsCache, the memoized result of
	// assembling runtimeInputs(). Building that value means constructing
	// the agent/agentic_fetch/custom-agent tool adapters and reading a
	// config snapshot, and runtimeInputs() is called several times per
	// turn (every credential-refresh path and every runtimeFor call goes
	// through it — see turn_dispatcher.go), so redoing that work on every
	// call was pure waste whenever nothing it depends on had changed.
	//
	// The cache is valid only while every signal it depends on is
	// unchanged: the config version (bumped by every config reload AND
	// every credential/account update, e.g. an OAuth refresh or a
	// rate-limit rotation — see config.ConfigStore.Version), the skills
	// generation (bumped by RefreshSkills), and the delegationTools
	// generation pointer (swapped by SetDelegationTools). Any one of
	// those changing forces a rebuild on the very next call, so a stale
	// tool set or skills snapshot is never handed to a build. Because
	// config version also changes on ordinary credential churn, this
	// mostly buys back the repeated calls within a single turn rather
	// than across turns — that is still the majority of the 20+ calls
	// this was written against.
	//
	// skillStates() is deliberately never cached here (see its own doc
	// comment on why it is read live) — every return, cached or not, is
	// refreshed with a live call before being handed back.
	runtimeInputsMu    sync.Mutex
	runtimeInputsCache *runtimeInputsCacheEntry
}

// skillsState owns the coordinator's skill discovery snapshot: the
// deduped/full skill set, the active subset, and the tracker that records
// which skills a run has loaded. mu guards all, active, tracker and gen.
// They start as a session-start snapshot, but RefreshSkills (called from
// the app's skills-directory watcher goroutine, internal/app/watch.go) can
// replace them mid-session while a Run is concurrently reading them via
// buildTools/logTurnSkillUsage — a plain field would race those reads. The
// tracker pointer itself is not replaced (see RefreshSkills), so its own
// internal lock is what protects loaded/activeNames; mu only protects
// which *slices*/tracker the coordinator hands out.
type skillsState struct {
	mu      sync.RWMutex
	all     []*skills.Skill // Pre-filter: all discovered after dedup.
	active  []*skills.Skill // Post-filter: active skills only.
	tracker *skills.Tracker
	// gen counts RefreshSkills calls. runtimeInputsCache keys its cached
	// skills snapshot on this rather than on all/active themselves, since
	// RefreshSkills always installs new slice headers — comparing slices
	// for identity would never hit, and comparing contents would redo the
	// sameSkills work RefreshSkills already did.
	gen uint64
	// mgr is the workspace's own skills manager, kept only to read the
	// discovery state snapshot (which SKILL.md files failed to parse or
	// validate) for sennit_info's [problems] section. It is read live
	// rather than snapshotted alongside the slices above because the
	// manager already owns the hot-reload path — its States() is current
	// by construction — and because it needs no lock of ours: the manager
	// guards its own snapshot. Nil for the legacy callers that construct a
	// coordinator without a manager (see NewCoordinator), in which case
	// there is simply no discovery state to report.
	mgr *skills.Manager
}

// fetchClient lazily builds and caches the *http.Client the agentic-fetch
// tool falls back to when no caller-supplied client is given. Built once
// and reused: cloning a transport per call would leak that clone's
// idle-connection pool forever and start every fetch cold instead of
// reusing warm connections from a shared pool.
type fetchClient struct {
	once   sync.Once
	client *http.Client
}

// get returns the shared *http.Client, constructing it on first use. An
// *http.Client is safe for concurrent use, so every caller after the first
// gets the same instance and shares its connection pool. Built via
// tools.NewHTTPClient rather than a second hand-rolled transport: the pool
// settings were already identical, and NewHTTPClient additionally guards
// against a redirect carrying this model-supplied URL to an address the
// user never approved (see checkFetchRedirect in tools.go) — a guard a
// second, separately-built transport would silently miss.
func (f *fetchClient) get() *http.Client {
	f.once.Do(func() {
		f.client = tools.NewHTTPClient(30 * time.Second)
	})
	return f.client
}

// runtimeInputsCacheEntry is one memoized runtimeInputs() result, along
// with the exact signal values it was built from (see
// delegationFinalizer.runtimeInputsMu).
type runtimeInputsCacheEntry struct {
	configVersion   uint64
	skillsGen       uint64
	delegationTools *delegationToolsSnapshot
	inputs          runtimeToolInputs
}

func (d *delegationFinalizer) runtimeInputs() runtimeToolInputs {
	allSkills, activeSkills, skillTracker, skillsGen := d.skillsSnapshotWithGen()
	delegationTools := d.delegationTools.Load()
	var configVersion uint64
	if d.cfg != nil {
		configVersion = d.cfg.Version()
	}

	if inputs, ok := d.cachedRuntimeInputs(configVersion, skillsGen, delegationTools); ok {
		inputs.skillStates = d.skillStates()
		return inputs
	}

	backgroundAgentsOn := true
	if d.cfg != nil {
		backgroundAgentsOn = d.backgroundAgentsEnabled()
	}
	var delegationToolsVal delegationToolsSnapshot
	if delegationTools != nil {
		delegationToolsVal = *delegationTools
	}
	inputs := runtimeToolInputs{
		allSkills: allSkills, activeSkills: activeSkills, skillTracker: skillTracker,
		delegationTools: delegationToolsVal, backgroundAgentsOn: backgroundAgentsOn,
		permissions: d.permissions, questions: d.questions, lspManager: d.lspManager,
		fileHistory: newFileHistory(d.history), filetracker: newFileTracking(d.filetracker), background: d.background,
		sessions: d.sessions, skillStates: d.skillStates(),
	}
	if d.cfg == nil {
		return inputs
	}
	buildCtx := context.Background()
	cfg := newAgentConfig(d.cfg.Config())
	agentTool, err := d.agentTool(buildCtx, cfg, true)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	// A delegated agent gets the same tool without the named-agent roster
	// - see agentTool's allowNamedAgents. Two instances rather than one
	// runtime check because the roster is half the description, and a
	// caller must not be shown agents it may not start.
	subAgentAgentTool, err := d.agentTool(buildCtx, cfg, false)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	agenticFetchTool, err := d.agenticFetchTool(buildCtx, nil)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	inputs.delegationToolsBuilt = map[string]fantasy.AgentTool{
		AgentToolName: agentTool, subAgentAgentToolKey: subAgentAgentTool,
		tools.AgenticFetchToolName: agenticFetchTool,
		"ask_parent":               tools.NewAskParentTool(d),
	}

	// Only a successful build is worth remembering: a transient failure
	// (e.g. the web_search backend construction erroring) should be
	// retried on the very next call, not stuck until an unrelated signal
	// changes.
	d.storeRuntimeInputsCache(configVersion, skillsGen, delegationTools, inputs)
	return inputs
}

// cachedRuntimeInputs returns the cached runtimeInputs() result if it was
// built from exactly this (configVersion, skillsGen, delegationTools)
// combination, and reports whether it did.
func (d *delegationFinalizer) cachedRuntimeInputs(configVersion, skillsGen uint64, delegationTools *delegationToolsSnapshot) (runtimeToolInputs, bool) {
	d.runtimeInputsMu.Lock()
	defer d.runtimeInputsMu.Unlock()
	cached := d.runtimeInputsCache
	if cached == nil || cached.configVersion != configVersion || cached.skillsGen != skillsGen || cached.delegationTools != delegationTools {
		return runtimeToolInputs{}, false
	}
	return cached.inputs, true
}

func (d *delegationFinalizer) storeRuntimeInputsCache(configVersion, skillsGen uint64, delegationTools *delegationToolsSnapshot, inputs runtimeToolInputs) {
	d.runtimeInputsMu.Lock()
	d.runtimeInputsCache = &runtimeInputsCacheEntry{
		configVersion: configVersion, skillsGen: skillsGen,
		delegationTools: delegationTools, inputs: inputs,
	}
	d.runtimeInputsMu.Unlock()
}

func (d *delegationFinalizer) invalidate(ctx context.Context, reason string, mutate func() bool) {
	d.builder.invalidateRuntime(ctx, reason, mutate)
}

func (d *delegationFinalizer) resolveAgentModel(ctx context.Context, agent config.Agent, isSubAgent bool) (Model, error) {
	return d.builder.buildAgentModel(ctx, agent, isSubAgent)
}

func (d *delegationFinalizer) resolveWebSearchBackend() (tools.SearchBackend, error) {
	return d.builder.webSearchBackend()
}

func (d *delegationFinalizer) newSubAgent(ctx context.Context, p *prompt.Prompt, agentCfg config.Agent) (SessionAgent, error) {
	return d.buildAgent(ctx, p, agentCfg, true)
}

func (d *delegationFinalizer) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	primary, err := d.builder.buildAgentModel(ctx, agent, isSubAgent)
	if err != nil {
		return nil, err
	}

	// Model is a value and ModelCfg a plain struct, so this override stays
	// local to the agent's copy and leaves the shared selected-model config
	// alone. effectiveReasoningEffort validates it against the model's levels
	// on every call and falls back to the model default when unsupported.
	if agent.ReasoningEffort != "" {
		primary.ModelCfg.ReasoningEffort = agent.ReasoningEffort
	}

	providerCfg, _ := d.cfg.Config().Providers.Get(primary.ModelCfg.Provider)
	result := NewSessionAgent(SessionAgentOptions{
		Model:                primary,
		SystemPromptPrefix:   providerCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: d.cfg.Config().Options.DisableAutoSummarize,
		AutoSummarizeAt:      d.cfg.Config().Options.AutoSummarizeAt,
		Sessions:             d.sessions,
		Messages:             d.messages,
		Tools:                nil,
		Lifecycle:            d.lifecycle,
		Notify:               d.notify,
		RunComplete:          d.runComplete,
		MCP:                  d.mcp,
		Latency:              d.latency,
	})

	// The readiness goroutines below perform one-time setup — building the
	// system prompt and the (MCP-gated) tool list — whose results the
	// coordinator needs for its whole lifetime, so they must survive the
	// caller's context being canceled. Some entry points build an agent
	// from a short-lived caller context, notably UpdateModels ->
	// buildTools -> agentTool -> buildAgent for the sub-agent. Because
	// mcp.WaitForInit blocks until MCP initialization finishes, a slow MCP
	// server keeps one of these goroutines parked past the call; if it
	// inherited the caller's cancellation, that context going away (or, for
	// the sub-agent rebuilt on every run, the *next* run's context replacing
	// this one) would abort the work before emitting anything — the session
	// hangs with no visible response. c.lifecycleCtx
	// (see ensureLifecycle) is scoped to the coordinator itself instead: it
	// only cancels when the coordinator is Close()d, so the work keeps
	// running until it either finishes or the coordinator shuts down —
	// bounding the git/MCP subprocesses it may spawn (see
	// internal/agent/prompt) to the coordinator's lifetime instead of
	// leaking them past it (e.g. past a test's t.TempDir cleanup).
	// ready is the errgroup these two goroutines report through.
	// d.readyWg is reserved for the coordinator's own top-level agent,
	// built exactly once in NewCoordinator: run()'s preamble waits on it
	// before the first turn. Sub-agents (isSubAgent=true), by contrast,
	// are rebuilt from scratch on every UpdateModels -> buildTools ->
	// agentTool pass, i.e. on every coordinator.Run call. Sharing
	// d.readyWg across those rebuilds raced sync.WaitGroup's Add against
	// a concurrent run's Wait once threads started dispatching multiple
	// Run calls at once on the same coordinator (Manager.dispatch, fired
	// from both Create and Send with no coordinator-level serialization),
	// and — independent of concurrency — errgroup.Group only remembers
	// its first error, so one failed sub-agent rebuild would permanently
	// fail readyWg.Wait() for every later run. A local group per build
	// keeps each sub-agent's readiness self-contained; nothing needs to
	// block on it, so failures are logged instead of propagated.
	ready := &d.lifecycle.primary
	if isSubAgent {
		ready = &errgroup.Group{}
	}

	if !d.lifecycle.launch(ready, func(initCtx context.Context) error {
		systemPrompt, err := prompt.Build(initCtx, primary.Model.Provider(), primary.Model.Model(), d.cfg)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	}) {
		return result, nil
	}

	if !d.lifecycle.launch(ready, func(initCtx context.Context) error {
		// Wait for MCP servers to finish registering their tools before
		// building the initial tool list. This ensures the first tool set
		// (used if anything reads it before run() rebuilds) includes all
		// MCP tools, not just fast-to-init ones.
		if err := d.builder.waitForMCPInit(initCtx); err != nil {
			return err
		}
		tools, err := d.builder.buildTools(initCtx, agent, isSubAgent, d.runtimeInputs())
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	}) {
		return result, nil
	}

	if isSubAgent {
		// runSubAgent waits on this before dispatching, so a delegation
		// can no longer start on an agent whose system prompt and tools
		// have not landed yet — a real possibility, since sub-agents are
		// rebuilt when the effective runtime key changes; a tool call can
		// follow one immediately. The goroutine stays for the case
		// nothing ever delegates to this build: a failure is worth a log
		// line even when no caller is left to receive it.
		if sa, ok := result.(*sessionAgent); ok {
			sa.subReady = ready
		}
		go func() {
			if err := ready.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("Failed to prepare sub-agent", "agent", agent.Name, "error", err)
			}
		}()
	}

	return result, nil
}

// operationPort assembles the {agent, inputs} pair every credential-refresh
// and rotation call needs: the coordinator's current top-level agent (for
// UpdateModels) and this generation's runtime inputs (for rebuilding a
// runtime after a refresh). Both turnDispatcher and delegationFinalizer
// share the same agentPort pointer (see NewCoordinator), so this is the one
// place that composes it rather than every call site rebuilding it.
func (d *delegationFinalizer) operationPort() runtimeOperationPort {
	return runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()}
}

// SetDelegationTools atomically publishes the thread and task tool
// adapters as one generation, invalidating the runtime when the thread
// adapter's identity changed (it changes the thread_* tool set; task
// adapters take effect immediately for background delegation).
func (d *delegationFinalizer) SetDelegationTools(threads tools.ThreadManager, tasks tools.TaskManager) {
	identity := managerIdentityOf(threads)
	d.invalidate(context.Background(), "threads_changed", func() bool {
		current := d.delegationTools.Load()
		d.delegationTools.Store(&delegationToolsSnapshot{
			threads: threads, tasks: tasks, threadsIdentity: identity,
		})
		return current == nil || !current.threadsIdentity.same(identity)
	})
}

// RefreshSkills replaces the cached skill discovery results — called by
// app.startExternalChangeWatchers after its skills-directory watcher
// detects a SKILL.md added, edited, or removed outside this process, so a
// hot-reload takes effect on the next Run without a restart. It preserves
// the skill tracker's loaded-state for names that are still active rather
// than resetting it, so a skill already read earlier in the session does
// not appear to forget itself.
func (d *delegationFinalizer) RefreshSkills(allSkills, activeSkills []*skills.Skill) {
	d.invalidate(context.Background(), "skills_changed", func() bool {
		d.skills.mu.Lock()
		changed := !sameSkills(d.skills.all, allSkills) || !sameSkills(d.skills.active, activeSkills)
		d.skills.all = allSkills
		d.skills.active = activeSkills
		// Bumped on every call, not only when changed: RefreshSkills
		// always installs new slice headers, so runtimeInputs' cache (keyed
		// on this) must treat every call as a potential change too, even
		// one sameSkills would call a no-op.
		d.skills.gen++
		// The tracker itself is not replaced: UpdateActiveSkills mutates it
		// in place under its own lock, keeping loaded state for names still
		// active rather than wiping it (see UpdateActiveSkills).
		tracker := d.skills.tracker
		d.skills.mu.Unlock()
		if tracker != nil {
			tracker.UpdateActiveSkills(activeSkills)
		}
		return changed
	})
}

// skillStates returns the workspace's current skill discovery states, or
// nil when no skills manager was wired in. Used for sennit_info's
// [problems] section, so an agent can see that a SKILL.md it was told to
// follow never loaded — see config.SkillProblems for why that particular
// failure is worth surfacing to the agent and not only to the log.
func (d *delegationFinalizer) skillStates() []*skills.SkillState {
	if d.skills.mgr == nil {
		return nil
	}
	return d.skills.mgr.States()
}

// skillsSnapshot returns the current skill discovery results under
// skillsMu, for callers (buildTools, Run) that need a consistent read
// while RefreshSkills may be running concurrently.
func (d *delegationFinalizer) skillsSnapshot() (allSkills, activeSkills []*skills.Skill, tracker *skills.Tracker) {
	allSkills, activeSkills, tracker, _ = d.skillsSnapshotWithGen()
	return allSkills, activeSkills, tracker
}

// skillsSnapshotWithGen is skillsSnapshot plus skillsGen, read under the
// same lock so the two can never observe two different RefreshSkills
// calls (see runtimeInputs' cache, the only caller that needs gen).
func (d *delegationFinalizer) skillsSnapshotWithGen() (allSkills, activeSkills []*skills.Skill, tracker *skills.Tracker, gen uint64) {
	d.skills.mu.RLock()
	defer d.skills.mu.RUnlock()
	return d.skills.all, d.skills.active, d.skills.tracker, d.skills.gen
}

// delegationToolsForRead returns a complete adapter generation.
func (d *delegationFinalizer) delegationToolsForRead() delegationToolsSnapshot {
	if snapshot := d.delegationTools.Load(); snapshot != nil {
		return *snapshot
	}
	return delegationToolsSnapshot{}
}

// tasksManager returns the task adapter from one delegation generation.
func (d *delegationFinalizer) tasksManager() tools.TaskManager {
	return d.delegationToolsForRead().tasks
}

// backgroundAgentsEnabled reports whether options.background_agents allows
// *new* background dispatch right now. It is read fresh on every call —
// never cached — so a config reload takes effect for the next dispatch
// without touching a task that is already running: that task's own runtime
// state lives in the task manager, not here, and this gate is never
// consulted again once a task has started.
func (d *delegationFinalizer) backgroundAgentsEnabled() bool {
	enabled := d.cfg.Config().Options.BackgroundAgents
	return enabled == nil || *enabled
}

// markSubSessionBusy records that a delegation is running under
// sessionID and returns the release to call when it finishes. The
// release is idempotent, and the counter (rather than a set) keeps a
// re-run of the same delegation — a retried tool call reuses the
// session id, which is derived from the message and tool-call ids —
// from clearing the flag out from under the run still in flight.
func (d *delegationFinalizer) markSubSessionBusy(sessionID string) func() {
	d.subSessionsMu.Lock()
	if d.subSessions == nil {
		d.subSessions = make(map[string]int)
	}
	d.subSessions[sessionID]++
	d.subSessionsMu.Unlock()

	var released bool
	return func() {
		if released {
			return
		}
		released = true
		d.subSessionsMu.Lock()
		defer d.subSessionsMu.Unlock()
		if d.subSessions[sessionID] <= 1 {
			delete(d.subSessions, sessionID)
			return
		}
		d.subSessions[sessionID]--
	}
}

// launchDelegation creates a durable background delegation through the
// workspace's task manager, enforcing the background-agents gate and the
// cascade depth limit. Completion is delivered independently by the task
// lifecycle (see TaskCompletion), never by keeping the caller's tool
// invocation open.
func (d *delegationFinalizer) launchDelegation(ctx context.Context, args tools.TaskCreateArgs) (fantasy.ToolResponse, error) {
	if !d.backgroundAgentsEnabled() {
		return fantasy.NewTextErrorResponse("Delegation is disabled in this workspace (options.background_agents)."), nil
	}
	depth := tools.GetDepthFromContext(ctx)
	if depth >= maxTaskCascadeDepth {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Delegation nesting limit (%d levels below the person) reached; do this work here instead of delegating it further.",
			maxTaskCascadeDepth,
		)), nil
	}
	manager := d.tasksManager()
	if manager == nil {
		return fantasy.NewTextErrorResponse("Delegation is unavailable in this workspace."), nil
	}
	// The depth the delegation itself runs at: one level below the turn
	// starting it. Carried on the task record, handed to the delegation's
	// own turns, and reported back on its completion — see
	// TaskCompletion.Depth and runTurn.foldCompletions.
	args.Depth = depth + 1
	info, err := manager.Create(ctx, args)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to start delegation: %s", err)), nil
	}
	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(fmt.Sprintf("Started delegation %s (session %s, status=%s). Its result will follow separately.", info.ID, info.SessionID, info.Status)),
		AgentBackgroundResponseMetadata{TaskID: info.ID, SessionID: info.SessionID, Status: info.Status},
	), nil
}

func (d *delegationFinalizer) runBackgroundAgent(ctx context.Context, sessionID, delegatedPrompt, title, childSessionID string, childDepth int) (fantasy.ToolResponse, error) {
	if title == "" {
		title = "New Agent Session"
	}
	return d.launchDelegation(ctx, tools.TaskCreateArgs{
		Goal:            delegatedPrompt,
		ParentSessionID: sessionID,
		SessionTitle:    title,
		SessionID:       childSessionID,
		Factory: func(ctx context.Context, childSessionID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
			agentCfg, ok := d.cfg.Config().Agents[config.AgentTask]
			if !ok {
				return nil, nil, errors.New("task agent not configured")
			}
			p, err := taskPrompt(prompt.WithWorkingDir(d.cfg.WorkingDir()))
			if err != nil {
				return nil, nil, err
			}
			agent, err := d.newSubAgent(ctx, p, agentCfg)
			if err != nil {
				return nil, nil, err
			}
			// Anonymous: this is the built-in `agent` tool's own
			// stateless delegate, not a named agent - see subAgentTaskRun.
			return d.subAgentTaskRun(sessionID, childSessionID, delegatedPrompt, agent, childDepth, ""), nil, nil
		},
	})
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (d *delegationFinalizer) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	sessionID, err := d.resolveSubAgentSessionID(ctx, params)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}

	// The delegate is built with its system prompt and tools assembled on
	// the build's own goroutines; a delegation dispatched before those
	// land runs with an empty prompt and no tools at all. Sub-agents are
	// rebuilt on every runtime invalidation, so a tool call arriving just
	// after one is exactly when that happens.
	//
	// This wait happens *before* the carried history is assembled: the
	// carry-over budget is sized from the delegate's actual system prompt,
	// tool schemas and this delegation's prompt (see carryOverBudget), so
	// those have to be final first. Waiting here rather than after the
	// carry-over means the budget is computed from the real runtime, not
	// from a guess, and the run still waits on the same readiness group it
	// always did - nothing is waited on twice, because the group's Wait is
	// idempotent and cheap once it has resolved.
	if waiter, ok := params.Agent.(interface {
		waitReady(context.Context) error
	}); ok {
		if err := waiter.waitReady(ctx); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("agent not ready: %w", err)
		}
	}

	// Capture one immutable runtime after readiness. The same value sizes
	// carry-over and drives Stream, so mutable agent/MCP state cannot drift
	// between those two operations.
	runtime := snapshotSubAgentRuntime(params.Agent, sessionID)

	// What this named agent already knows, from its earlier delegations
	// under the same parent. Collected after the session exists so the
	// query can exclude it by id, and treated as best-effort: a
	// delegation that has lost its memory is worse than one that
	// remembers, but far better than one that refuses to run.
	//
	// The budget is sized from the model and the concrete runtime: the
	// delegate's context window and output capacity, plus the actual byte
	// sizes of the system prompt, tool schemas and this delegation's
	// prompt, all read now that the build has landed.
	model := params.Agent.Model()
	if runtime != nil {
		model = runtime.model
	}
	priorMessages, err := d.subAgentCarryOverMessages(ctx, params, sessionID, model, runtime)
	if err != nil {
		slog.Warn(
			"Failed to carry over sub-agent history; running without it",
			"agent", params.AgentID,
			"parent_session", params.SessionID,
			"child_session", sessionID,
			"error", err,
		)
	}

	// Get model configuration
	maxTokens := modelMaxOutputTokens(model)

	cfg := d.cfg.Config()
	providerCfg, ok := cfg.Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}
	providerCredentials, ok := cfg.RuntimeProvider(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	// A sub-agent's turn dispatches through t.model (see modelProvider in
	// turn.go) unless its call carries an ActiveRuntime that a refresh has
	// populated. Without one, a 401 mid-delegation refreshed the
	// credential in config but kept retrying with the provider instance
	// built from the *old* one - the delegation died on an expiry the
	// top-level agent recovers from cleanly. active starts empty and is
	// only ever populated by a successful refresh (see
	// makeSubAgentAuthRefreshCallback, which rebuilds the runtime for this
	// delegation's own model rather than the coordinator's - see its
	// comment for why that distinction matters); it is local to this call
	// and discarded with it, so it cannot leak into the parent's turn or a
	// later delegation.
	active := newActiveRuntime(nil)

	// Run the agent. Takes its context explicitly - the non-detached path
	// below runs it with ctx directly, the detachable path with a child
	// context that can outlive ctx.
	run := func(runCtx context.Context) (*fantasy.AgentResult, error) {
		call := d.buildSubAgentCall(params, sessionID, priorMessages, maxTokens, model, providerCfg, providerCredentials, active)
		if runtimeAgent, ok := params.Agent.(interface {
			runWithStreamRuntime(context.Context, SessionAgentCall, streamRuntime) (*fantasy.AgentResult, error)
		}); ok && runtime != nil {
			return runtimeAgent.runWithStreamRuntime(runCtx, call, *runtime)
		}
		return params.Agent.Run(runCtx, call)
	}

	// Report the child session as busy for as long as it is running:
	// nothing else can, since the delegate's dispatcher is not the one
	// the coordinator asks. See markSubSessionBusy. Exactly one of the
	// two branches below calls releaseBusy, and each calls it exactly
	// once.
	releaseBusy := d.markSubSessionBusy(sessionID)
	result, err := run(ctx)
	releaseBusy()
	// A run cancelled by the caller's context (app shutdown, a parent run
	// being torn down, an explicit TaskManager.Cancel racing this one) must
	// come back as a Go error, not as finishSubAgent's text-error response:
	// folding it into text loses the sentinel, and subAgentTaskRun would
	// rebuild it with errors.New(resp.Content), which thread.lifecycle's
	// errors.Is(err, context.Canceled) check can no longer match — the
	// delegation is then finalized as failed instead of cancelled. A model
	// or provider failure has no such sentinel to preserve, so it still
	// goes through finishSubAgent below, unchanged.
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil) {
		return fantasy.ToolResponse{}, err
	}
	return d.finishSubAgent(subAgentOutcome{result: result, err: err}), nil
}

// resolveSubAgentSessionID returns the child session id a delegation
// should run in: the caller-supplied ChildSessionID for a task launch
// (already created and finalized atomically by the store), or a freshly
// created sub-agent session for a legacy direct call.
func (d *delegationFinalizer) resolveSubAgentSessionID(ctx context.Context, params subAgentParams) (string, error) {
	if params.ChildSessionID != "" {
		return params.ChildSessionID, nil
	}
	agentToolSessionID := session.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
	session, err := d.sessions.CreateSubAgentSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle, params.AgentID)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return session.ID, nil
}

// snapshotSubAgentRuntime captures the delegate's immutable runtime after
// readiness, if it exposes one, so the same value sizes carry-over and
// later drives Stream. Returns nil when params.Agent doesn't implement
// the snapshot seam (see stream_runtime_snapshot_test.go).
func snapshotSubAgentRuntime(agent SessionAgent, sessionID string) *streamRuntime {
	snap, ok := agent.(interface {
		snapshotStreamRuntime(SessionAgentCall) streamRuntime
	})
	if !ok {
		return nil
	}
	captured := snap.snapshotStreamRuntime(SessionAgentCall{SessionID: sessionID})
	return &captured
}

// subAgentCarryOverMessages sizes the carry-over budget from the resolved
// model and runtime (falling back to the agent's own runtimeSnapshot when
// no streamRuntime was captured) and fetches the prior messages this
// delegation should see ahead of its own.
func (d *delegationFinalizer) subAgentCarryOverMessages(ctx context.Context, params subAgentParams, sessionID string, model Model, runtime *streamRuntime) ([]message.Message, error) {
	budgetIn := carryOverBudgetInput{
		Model:             model,
		SystemPromptBytes: 0,
		ToolSchemaBytes:   0,
		PromptBytes:       len(params.Prompt),
	}
	if runtime != nil {
		budgetIn.SystemPromptBytes = len(runtime.systemPrompt) + len(runtime.systemPromptPrefix)
		budgetIn.ToolSchemaBytes = toolSchemaBytes(runtime.tools)
	} else if snap, ok := params.Agent.(interface {
		runtimeSnapshot(SessionAgentCall) (string, []fantasy.AgentTool)
	}); ok {
		systemPrompt, agentTools := snap.runtimeSnapshot(SessionAgentCall{SessionID: sessionID})
		budgetIn.SystemPromptBytes = len(systemPrompt)
		budgetIn.ToolSchemaBytes = toolSchemaBytes(agentTools)
	}
	return d.carryOverMessages(ctx, budgetIn, params.SessionID, params.AgentID, sessionID)
}

// buildSubAgentCall assembles the SessionAgentCall a delegation's run
// closure sends to params.Agent. active is threaded straight through as
// ActiveRuntime and into makeSubAgentAuthRefreshCallback - the exact wiring
// a successful mid-delegation credential refresh depends on (see
// runSubAgent's own comment on active) - so this must stay a plain
// pass-through, never a copy or a fresh instance.
//
// OnAuthRefresh uses makeSubAgentAuthRefreshCallback, not
// makeAuthRefreshCallback: see that function's own comment for why the
// sub-agent's own model, not the coordinator's, has to drive what a
// refresh stores into active.
//
// OnRateLimit and RotateThreshold get the same treatment, for the same
// reason: makeSubAgentRateLimitCallback/makeSubAgentThresholdRotateCallback
// rebuild through buildSubAgentRuntime(model), never runtimeFor(inputs), so
// a rotation mid-delegation lands the delegate back on its own model/account
// instead of quietly upgrading it to the coordinator's full runtime -
// exactly the escalation agentTool(allowNamedAgents=false) already refuses
// through the tool set. Without this wiring, a 429 or an over-threshold
// account on a delegation used to surface immediately as a failed
// delegation instead of rotating and continuing, even when rotation was
// configured and would have recovered the parent turn.
func (d *delegationFinalizer) buildSubAgentCall(params subAgentParams, sessionID string, priorMessages []message.Message, maxTokens int64, model Model, providerCfg config.ProviderConfig, cred providerstate.Provider, active *activeRuntime) SessionAgentCall {
	return SessionAgentCall{
		SessionID:       sessionID,
		Depth:           params.Depth,
		Prompt:          params.Prompt,
		PriorMessages:   priorMessages,
		MaxOutputTokens: maxTokens,
		// Keyed on the child session: a delegation's ~90 steps all
		// replay its own growing prefix, which is exactly the run of
		// requests prompt_cache_key exists to keep together.
		ProviderOptions:  withPromptCacheKey(getProviderOptions(model, providerCfg), model, providerCfg, sessionID),
		Temperature:      model.ModelCfg.Temperature,
		TopP:             model.ModelCfg.TopP,
		TopK:             model.ModelCfg.TopK,
		FrequencyPenalty: model.ModelCfg.FrequencyPenalty,
		PresencePenalty:  model.ModelCfg.PresencePenalty,
		NonInteractive:   true,
		ActiveRuntime:    active,
		OnAuthRefresh:    d.builder.makeSubAgentAuthRefreshCallback(providerCfg, cred, model, active, d.operationPort()),
		OnRateLimit:      d.builder.makeSubAgentRateLimitCallback(providerCfg, cred, model, active),
		RotateThreshold:  d.builder.makeSubAgentThresholdRotateCallback(providerCfg, cred, model, active),
	}
}

// subAgentTaskRun builds the closure a task's Factory hands back to run the
// delegation. agentID must be the *named* agent's config id for a
// delegation to a named agent, and empty for the anonymous `agent` and
// `agentic_fetch` delegations (see subAgentParams.AgentID) - it is threaded
// straight into subAgentParams so carryOverMessages can find that agent's
// earlier sessions under this parent.
//
// Setting it turns on real, ongoing cost: every delegation to that named
// agent now pays to replay its earlier conversations under the same
// parent (trimmed to the carry-over budget in subagent_memory.go), which is
// input tokens on every call, not just the first.
func (d *delegationFinalizer) subAgentTaskRun(parentSessionID, childSessionID, prompt string, agent SessionAgent, depth int, agentID string) func(context.Context) (tools.TaskRunResult, error) {
	return func(ctx context.Context) (tools.TaskRunResult, error) {
		resp, err := d.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSessionID,
			ChildSessionID: childSessionID,
			Prompt:         prompt,
			Depth:          depth,
			AgentID:        agentID,
		})
		if err != nil {
			return tools.TaskRunResult{}, err
		}
		if resp.IsError {
			return tools.TaskRunResult{}, errors.New(resp.Content)
		}
		return tools.TaskRunResult{Text: resp.Content}, nil
	}
}

// finishSubAgent turns a completed child run into a terminal task result.
// Asynchronous task runs are finalized transactionally by thread.lifecycle;
// this synchronous helper intentionally does not attribute cost.
func (d *delegationFinalizer) finishSubAgent(outcome subAgentOutcome) fantasy.ToolResponse {
	if outcome.err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to generate response: %s", outcome.err))
	}
	output := subAgentOutput(outcome.result)
	if output == "" {
		return fantasy.NewTextErrorResponse("Sub-agent completed but produced no text output.")
	}
	return fantasy.NewTextResponse(output)
}

// carryOverMessages collects the named sub-agent's earlier conversations
// under the same parent, trimmed to the carried-history budget.
func (d *delegationFinalizer) carryOverMessages(ctx context.Context, in carryOverBudgetInput, parentSessionID, agentID, currentSessionID string) ([]message.Message, error) {
	if agentID == "" {
		return nil, nil
	}

	prior, err := d.sessions.ListSubAgentSessions(ctx, parentSessionID, agentID, currentSessionID)
	if err != nil {
		return nil, fmt.Errorf("list prior %s sessions: %w", agentID, err)
	}
	if len(prior) == 0 {
		return nil, nil
	}

	// Per-session slices, kept apart until the budget has been applied:
	// the budget drops whole sessions, never half of an exchange.
	perSession := make([][]message.Message, 0, len(prior))
	for _, s := range prior {
		msgs, err := d.messages.List(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("list messages of prior %s session %s: %w", agentID, s.ID, err)
		}
		msgs = trimToSummary(s, msgs)
		if len(msgs) == 0 {
			continue
		}
		perSession = append(perSession, msgs)
	}

	budget := carryOverBudget(in)
	// The trim is correlated to the PARENT session (the one whose sub-agent
	// history is being carried) and the current run, so the chain tool can
	// group it with the run's provider/repair lines by session_id/run_id.
	kept, dropped := applyCarryOverBudget(perSession, budget, trimCorr(parentSessionID, RunIDFromContext(ctx)))
	if dropped > 0 {
		slog.Info(
			"Dropped oldest sub-agent sessions from carried history",
			"agent", agentID,
			"parent_session", parentSessionID,
			"dropped_sessions", dropped,
			"kept_sessions", len(perSession)-dropped,
			"budget_bytes", budget,
			"context_window", in.Model.CatalogCfg.ContextWindow,
		)
	}
	return kept, nil
}

// agentTool builds the built-in "agent" delegation tool: every call opens
// a durable background task (see runBackgroundAgent), so the tool is only
// available when the workspace owns a task manager and background
// delegation is enabled. allowNamedAgents is false for a sub-agent's own
// build: a delegation could never start a named agent (their tools were
// registered for the top-level agent only), and folding them into one
// tool must not quietly widen that.
func (d *delegationFinalizer) agentTool(_ context.Context, cfg agentConfig, allowNamedAgents bool) (fantasy.AgentTool, error) {
	if _, ok := cfg.Agents()[config.AgentTask]; !ok {
		return nil, errors.New("task agent not configured")
	}
	var named []namedAgent
	if allowNamedAgents {
		named = namedAgents(cfg)
	}
	constraints := map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}
	if len(named) > 0 {
		ids := make([]string, len(named))
		for i, a := range named {
			ids[i] = a.ID
		}
		constraints["subagent_type"] = tools.ToolSchemaConstraint{Enum: ids}
	}
	return tools.WithToolSchemaConstraints(fantasy.NewAgentTool(
		AgentToolName,
		agentToolDescription(named),
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			if params.SubagentType == "" {
				return d.runBackgroundAgent(ctx, sessionID, params.Prompt, params.Description, delegationSessionID(ctx, call.ID), delegationDepth(ctx))
			}
			if !allowNamedAgents {
				return fantasy.NewTextErrorResponse(
					"subagent_type is not available to a delegated agent: delegate without it to " +
						"get the general-purpose agent, or do the work here."), nil
			}
			return d.runNamedAgent(ctx, sessionID, params, call)
		},
	), constraints), nil
}

// runNamedAgent starts params.SubagentType on params.Prompt. The config is
// re-read here rather than closed over: the tool outlives a config reload
// only until the next rebuild, and an agent deleted in between must be
// refused by name instead of started from a stale definition.
func (d *delegationFinalizer) runNamedAgent(ctx context.Context, parentID string, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	id := params.SubagentType
	latest, ok := d.cfg.Config().Agents[id]
	if !ok || !delegatableAgentID(id) {
		available := namedAgents(newAgentConfig(d.cfg.Config()))
		ids := make([]string, len(available))
		for i, a := range available {
			ids[i] = a.ID
		}
		if len(ids) == 0 {
			return fantasy.NewTextErrorResponse(fmt.Sprintf(
				"No agent %q, and no agents are configured in this workspace. Omit subagent_type to use the general-purpose agent.", id)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"No agent %q. Available: %s. Omit subagent_type to use the general-purpose agent.",
			id, strings.Join(ids, ", "))), nil
	}
	title := params.Description
	if title == "" {
		title = latest.Name
	}
	childDepth := delegationDepth(ctx)
	return d.launchDelegation(ctx, tools.TaskCreateArgs{
		Goal:            params.Prompt,
		ParentSessionID: parentID,
		SessionTitle:    title,
		AgentID:         id,
		SessionID:       delegationSessionID(ctx, call.ID),
		Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
			definition, ok := d.cfg.Config().Agents[id]
			if !ok {
				return nil, nil, fmt.Errorf("agent %q is no longer configured", id)
			}
			systemPrompt, err := prompt.NewPrompt(id, delegatedAgentPrompt(definition.Prompt), prompt.WithWorkingDir(d.cfg.WorkingDir()))
			if err != nil {
				return nil, nil, fmt.Errorf("parse prompt: %w", err)
			}
			agent, err := d.newSubAgent(ctx, systemPrompt, definition)
			if err != nil {
				return nil, nil, err
			}
			// Named: id is this delegation's target agent, so
			// carryOverMessages can replay its earlier sessions under
			// parentID - see subAgentTaskRun's doc comment for the cost
			// this switches on.
			return d.subAgentTaskRun(parentID, childID, params.Prompt, agent, childDepth, id), nil, nil
		},
	})
}

//nolint:unparam // matches the (tool, error) signature of the other buildTools helpers
func (d *delegationFinalizer) agenticFetchTool(_ context.Context, client *http.Client) (fantasy.AgentTool, error) {
	if client == nil {
		client = d.fetch.get()
	}
	return tools.WithToolSchemaConstraints(fantasy.NewAgentTool(
		tools.AgenticFetchToolName,
		agenticFetchToolDescription,
		func(ctx context.Context, params tools.AgenticFetchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			validation, invalid, err := validateAgenticFetchParams(ctx, params)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if invalid != nil {
				return fantasy.NewTextErrorResponse(invalid.Error()), nil
			}
			childDepth := delegationDepth(ctx)
			return d.launchDelegation(ctx, tools.TaskCreateArgs{
				Goal:            params.Prompt,
				ParentSessionID: validation.SessionID,
				SessionTitle:    "Fetch Analysis",
				SessionID:       session.CreateAgentToolSessionID(validation.AgentMessageID, call.ID),
				Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
					return d.agenticFetchFactory(ctx, client, params, validation, call, childDepth, childID)
				},
			})
		},
	), map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}), nil
}

// agenticFetchFactory is the agentic-fetch tool's TaskCreateArgs.Factory: it
// requests permission, fetches params.URL (if any) into a scratch
// directory, builds that delegation's own system prompt and sub-agent, and
// hands back its run function and cleanup. Split out of agenticFetchTool
// only to keep that closure's nesting shallow - behavior is unchanged.
//
// This builds its own NewSessionAgent by hand instead of going through
// buildAgent/buildTools, deliberately: every tool this delegate gets is
// rooted at tmpDir, a throwaway scratch directory, not the workspace's
// real working directory - that confinement is the whole point of running
// URL/search content through a disposable sub-agent rather than the
// caller's own turn. buildToolsCtx.runtimeCfg (and so every toolSpecs
// row) is always the workspace's own runtimeConfigSnapshot from
// b.runtimeConfigSnapshot() - there is no per-call override to point one
// delegation's tools at a different root - so routing through the shared
// registry would hand this delegate read/glob/grep/web_fetch/web_search
// (and, since the registry's core row is one grouped Build func, every
// other core tool too - bash, edit, write included) scoped to the real
// project instead of tmpDir. It would also wire web_fetch/web_search
// through the live permission.Requester the registry's row uses, which
// would prompt the user again for a fetch this delegation was already
// granted permission for by the outer agentic_fetch call - see
// TestAgenticFetchSubAgentView_OutsideWorkdirRequiresPermission for the
// read tool's own (already-live) permission wiring. Both are why this
// hand-builds its tool list rather than routing through buildAgent.
func (d *delegationFinalizer) agenticFetchFactory(ctx context.Context, client *http.Client, params tools.AgenticFetchParams, validation agenticFetchValidationResult, call fantasy.ToolCall, childDepth int, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
	description := "Search the web and analyze results"
	if params.URL != "" {
		description = fmt.Sprintf("Fetch and analyze content from URL: %s", params.URL)
	}
	allowed, err := d.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID: validation.SessionID, Path: d.cfg.WorkingDir(), ToolCallID: call.ID,
		ToolName: tools.AgenticFetchToolName, Action: "fetch", Description: description,
		Params: tools.AgenticFetchPermissionsParams(params),
	})
	if err != nil {
		return nil, nil, err
	}
	if !allowed {
		return nil, nil, errors.New("permission denied for agentic fetch")
	}
	tmpDir, err := os.MkdirTemp(d.cfg.Config().Options.DataDirectory, brand.Slug+"-fetch-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }
	fullPrompt := params.Prompt + "\n\nUse web_search and web_fetch to research this request."
	if params.URL != "" {
		content, filePath, err := tools.FetchLargeContent(ctx, client, tmpDir, params.URL)
		if err != nil {
			return nil, cleanup, fmt.Errorf("fetch URL: %w", err)
		}
		if filePath != "" {
			fullPrompt = fmt.Sprintf("%s\n\nThe web page from %s is saved at %s. Analyze it with read and grep.", params.Prompt, params.URL, filePath)
		} else {
			fullPrompt = fmt.Sprintf("%s\n\nWeb page URL: %s\n\n<webpage_content>\n%s\n</webpage_content>", params.Prompt, params.URL, content)
		}
	}
	agent, err := d.buildAgenticFetchAgent(ctx, client, tmpDir)
	if err != nil {
		return nil, cleanup, err
	}
	// Anonymous: agentic_fetch has no named-agent identity of its own.
	return d.subAgentTaskRun(validation.SessionID, childID, fullPrompt, agent, childDepth, ""), cleanup, nil
}

// buildAgenticFetchAgent builds the sandboxed sub-agent one agentic-fetch
// delegation runs on: its system prompt (from the agentic_fetch template,
// rooted at tmpDir) and its hand-picked, tmpDir-scoped tool set (see
// agenticFetchFactory's doc comment for why this can't go through
// buildAgent). Split out of agenticFetchFactory so the agent itself - the
// piece a test needs to check IsSubAgent and the tool set against - can be
// built and inspected without also running a permission request or a
// fetch.
func (d *delegationFinalizer) buildAgenticFetchAgent(ctx context.Context, client *http.Client, tmpDir string) (SessionAgent, error) {
	promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(agenticFetchPromptTmpl), prompt.WithWorkingDir(tmpDir))
	if err != nil {
		return nil, err
	}
	model, err := d.resolveAgentModel(ctx, config.Agent{}, true)
	if err != nil {
		return nil, err
	}
	systemPrompt, err := promptTemplate.Build(ctx, model.Model.Provider(), model.Model.Model(), d.cfg)
	if err != nil {
		return nil, err
	}
	providerCfg, ok := d.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return nil, errors.New("model provider not configured")
	}
	searchBackend, err := d.resolveWebSearchBackend()
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}
	availability := tools.ResolveSystemToolAvailability()
	return NewSessionAgent(SessionAgentOptions{
		Model: model, SystemPromptPrefix: providerCfg.SystemPromptPrefix, SystemPrompt: systemPrompt,
		// This delegate never goes through buildAgent (see
		// agenticFetchFactory's doc comment), so it must set IsSubAgent
		// itself: preparePrompt only skips the parent todo-reminder
		// injection (compat.go) for an agent explicitly marked as one, and
		// this scratch-dir analysis agent has neither a todo list nor a
		// "todos" tool to act on it with.
		IsSubAgent:           true,
		DisableAutoSummarize: d.cfg.Config().Options.DisableAutoSummarize,
		AutoSummarizeAt:      d.cfg.Config().Options.AutoSummarizeAt,
		Sessions:             d.sessions, Messages: d.messages,
		Lifecycle: d.lifecycle,
		Tools: []fantasy.AgentTool{
			tools.NewWebFetchTool(nil, tmpDir, tmpDir, client, availability), tools.NewWebSearchTool(nil, tmpDir, client, searchBackend, availability),
			tools.NewGlobTool(d.permissions, tmpDir, d.cfg.Config().Tools.Glob), tools.NewSearchTool(d.permissions, tmpDir, d.cfg.Config().Tools.Grep),
			tools.NewReadTool(d.lspManager, d.permissions, newFileTracking(d.filetracker), nil, tmpDir),
		},
	}), nil
}

// delegatableAgentID reports whether id names an agent a caller may hand
// work to. The two built-in roles are not: "coder" is the top-level agent
// itself, and "task" is what a caller gets by omitting subagent_type.
func delegatableAgentID(id string) bool {
	return id != config.AgentCoder && id != config.AgentTask
}

// namedAgents is the roster the agent tool advertises and validates
// against, sorted by id so the description is stable across rebuilds.
func namedAgents(cfg agentConfig) []namedAgent {
	agents := cfg.Agents()
	ids := make([]string, 0, len(agents))
	for id := range agents {
		if delegatableAgentID(id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	out := make([]namedAgent, 0, len(ids))
	for _, id := range ids {
		out = append(out, namedAgent{ID: id, Description: customAgentDescription(id, agents[id])})
	}
	return out
}

func customAgentDescription(id string, agentCfg config.Agent) string {
	if agentCfg.Description != "" {
		return agentCfg.Description
	}
	name := agentCfg.Name
	if name == "" {
		name = id
	}
	return fmt.Sprintf("Delegate a task to the %s agent.", name)
}

// SendToParent delivers a mid-run ask from sessionID to its registered
// parent. The ask_parent tool receives the finalizer as its
// tools.ParentMessenger: the finalizer routes through the coordinator's
// current agent, whose dispatcher holds the parent registration.
func (d *delegationFinalizer) SendToParent(ctx context.Context, sessionID, message string) error {
	agent := d.agentPort.current()
	if agent == nil {
		return nil
	}
	return agent.SendToParent(ctx, sessionID, message)
}
