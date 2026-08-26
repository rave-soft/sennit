package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
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
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    MessageService
	permissions permission.Service
	questions   question.Service
	history     history.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	background  *shell.BackgroundShellManager
	builder     *runtimeBuilder
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	mcp         *mcp.Registry
	latency     latency.Recorder

	lifecycle *readinessLifecycle

	// parentCostMu serializes updateParentSessionCost's read-modify-write
	// of a parent session's cost. Sub-agents of the same parent can
	// finish concurrently (e.g. several "agent" tool calls from the same
	// turn's tool batch, or nested delegations), and without this lock
	// their Get/Save pairs interleave: two updates that both read the
	// pre-update cost and each add their own delta drop one of the two
	// deltas on the floor.
	parentCostMu sync.Mutex

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

	// skillsMu guards allSkills/activeSkills/skillTracker. They start as a
	// session-start snapshot, but RefreshSkills (called from the app's
	// skills-directory watcher goroutine, internal/app/watch.go) can
	// replace them mid-session
	// while a Run is concurrently reading them via buildTools/
	// logTurnSkillUsage — a plain field would race those reads. The
	// skillTracker pointer itself is not replaced (see RefreshSkills), so
	// its own internal lock is what protects loaded/activeNames; this
	// mutex only protects which *slices*/tracker the coordinator hands out.
	skillsMu     sync.RWMutex
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker
	// skillsMgr is the workspace's own skills manager, kept only to read
	// the discovery state snapshot (which SKILL.md files failed to parse
	// or validate) for sennit_info's [problems] section. It is read live
	// rather than snapshotted alongside the slices above because the
	// manager already owns the hot-reload path — its States() is current
	// by construction — and because it needs no lock of ours: the manager
	// guards its own snapshot. Nil for the legacy callers that construct a
	// coordinator without a manager (see NewCoordinator), in which case
	// there is simply no discovery state to report.
	skillsMgr *skills.Manager

	agentPort *coordinatorAgentPort
}

func (d *delegationFinalizer) runtimeInputs() runtimeToolInputs {
	allSkills, activeSkills, skillTracker := d.skillsSnapshot()
	backgroundAgentsOn := true
	if d.cfg != nil {
		backgroundAgentsOn = d.backgroundAgentsEnabled()
	}
	inputs := runtimeToolInputs{
		allSkills: allSkills, activeSkills: activeSkills, skillTracker: skillTracker,
		delegationTools: d.delegationToolsForRead(), backgroundAgentsOn: backgroundAgentsOn,
		permissions: d.permissions, questions: d.questions, lspManager: d.lspManager,
		history: d.history, filetracker: d.filetracker, background: d.background,
		sessions: d.sessions, skillStates: d.skillStates(),
	}
	if d.cfg == nil {
		return inputs
	}
	buildCtx := context.Background()
	cfg := newAgentConfig(d.cfg.Config())
	agentTool, err := d.agentTool(buildCtx, cfg)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	agenticFetchTool, err := d.agenticFetchTool(buildCtx, nil)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	customAgentTools, err := d.customAgentTools(buildCtx, cfg)
	if err != nil {
		inputs.toolBuildErr = err
		return inputs
	}
	inputs.delegationToolsBuilt = map[string]fantasy.AgentTool{
		AgentToolName: agentTool, tools.AgenticFetchToolName: agenticFetchTool,
		"ask_parent": tools.NewAskParentTool(d),
	}
	inputs.customAgentToolsBuilt = customAgentTools
	return inputs
}

func (d *delegationFinalizer) invalidate(ctx context.Context, reason string, mutate func() bool) {
	d.builder.invalidateRuntime(ctx, reason, mutate)
}

func (d *delegationFinalizer) resolveAgentModel(ctx context.Context, isSubAgent bool) (Model, error) {
	return d.builder.buildAgentModel(ctx, isSubAgent)
}

func (d *delegationFinalizer) resolveWebSearchBackend() (tools.SearchBackend, error) {
	return d.builder.webSearchBackend()
}

func (d *delegationFinalizer) authRefreshCallback(providerCfg config.ProviderConfig) func(context.Context, *fantasy.ProviderError) error {
	return d.builder.makeAuthRefreshCallback(providerCfg, nil, runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()})
}

func (d *delegationFinalizer) newSubAgent(ctx context.Context, p *prompt.Prompt, agentCfg config.Agent) (SessionAgent, error) {
	return d.buildAgent(ctx, p, agentCfg, true)
}

func (d *delegationFinalizer) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	model, err := d.builder.buildAgentModel(ctx, isSubAgent)
	if err != nil {
		return nil, err
	}

	// An empty agent.Model means "inherit the app's main model", which is
	// the model built above. A non-empty value is a "provider/model-id"
	// string naming a specific model of its own.
	var primary Model
	if agent.Model == "" {
		primary = model
	} else {
		primary, err = d.builder.buildCustomAgentModel(ctx, agent, isSubAgent)
		if err != nil {
			return nil, err
		}
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

func (d *delegationFinalizer) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	return d.builder.buildTools(ctx, agent, isSubAgent, d.runtimeInputs())
}

func (d *delegationFinalizer) runtimeFor(ctx context.Context) (*compiledRuntime, error) {
	return d.builder.runtimeFor(ctx, d.runtimeInputs())
}

func (d *delegationFinalizer) makeAuthRefreshCallback(providerCfg config.ProviderConfig, active *activeRuntime) func(context.Context, *fantasy.ProviderError) error {
	return d.builder.makeAuthRefreshCallback(providerCfg, active, runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()})
}

func (d *delegationFinalizer) waitForInteractiveReauth(ctx context.Context, providerID string) error {
	return d.builder.waitForInteractiveReauth(ctx, providerID, runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()})
}

func (d *delegationFinalizer) refreshTokenIfExpired(ctx context.Context, cfg config.ProviderConfig) error {
	return d.builder.refreshTokenIfExpired(ctx, cfg, runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()})
}

func (d *delegationFinalizer) retryAfterUnauthorized(ctx context.Context, cfg config.ProviderConfig) error {
	return d.builder.retryAfterUnauthorized(ctx, cfg, runtimeOperationPort{agent: d.agentPort.current(), inputs: d.runtimeInputs()})
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
		d.skillsMu.Lock()
		changed := !sameSkills(d.allSkills, allSkills) || !sameSkills(d.activeSkills, activeSkills)
		d.allSkills = allSkills
		d.activeSkills = activeSkills
		// The tracker itself is not replaced: UpdateActiveSkills mutates it
		// in place under its own lock, keeping loaded state for names still
		// active rather than wiping it (see UpdateActiveSkills).
		tracker := d.skillTracker
		d.skillsMu.Unlock()
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
	if d.skillsMgr == nil {
		return nil
	}
	return d.skillsMgr.States()
}

// skillsSnapshot returns the current skill discovery results under
// skillsMu, for callers (buildTools, Run) that need a consistent read
// while RefreshSkills may be running concurrently.
func (d *delegationFinalizer) skillsSnapshot() (allSkills, activeSkills []*skills.Skill, tracker *skills.Tracker) {
	d.skillsMu.RLock()
	defer d.skillsMu.RUnlock()
	return d.allSkills, d.activeSkills, d.skillTracker
}

// delegationToolsForRead returns a complete adapter generation.
func (d *delegationFinalizer) delegationToolsForRead() delegationToolsSnapshot {
	if snapshot := d.delegationTools.Load(); snapshot != nil {
		return *snapshot
	}
	return delegationToolsSnapshot{}
}

// threadsManager returns the thread adapter from one delegation generation.
func (d *delegationFinalizer) threadsManager() tools.ThreadManager {
	return d.delegationToolsForRead().threads
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

// updateParentSessionCost accumulates the cost from a child session to its
// parent session.
//
// The accumulation is a single narrow UPDATE (cost = cost + delta), which
// is what makes it safe against every other writer of that row. The
// Get-modify-Save it replaces did not just race sibling delegations — it
// wrote the whole row back, so a turn's usage save or a todo write that
// landed between the read and the write was overwritten with the values
// this call had read before them. parentCostMu is kept: two siblings
// still read the child cost and issue their updates concurrently, and
// serialising them keeps the published session events in a sensible
// order.
func (d *delegationFinalizer) updateParentSessionCost(ctx context.Context, childSessionID, parentSessionID string) error {
	d.parentCostMu.Lock()
	defer d.parentCostMu.Unlock()

	childSession, err := d.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("get child session: %w", err)
	}

	if err := d.sessions.AddCost(ctx, parentSessionID, childSession.Cost); err != nil {
		return fmt.Errorf("get parent session: %w", err)
	}

	return nil
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

func (d *delegationFinalizer) runBackgroundAgent(ctx context.Context, sessionID, delegatedPrompt, childSessionID string, childDepth int) (fantasy.ToolResponse, error) {
	return d.launchDelegation(ctx, tools.TaskCreateArgs{
		Goal:            delegatedPrompt,
		ParentSessionID: sessionID,
		SessionTitle:    "New Agent Session",
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
			return d.subAgentTaskRun(sessionID, childSessionID, delegatedPrompt, agent, childDepth), nil, nil
		},
	})
}

// runSubAgent runs a sub-agent and handles session management and cost accumulation.
// It creates a sub-session, runs the agent with the given prompt, and propagates
// the cost to the parent session.
func (d *delegationFinalizer) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	var sessionID string
	if params.ChildSessionID != "" {
		sessionID = params.ChildSessionID
	} else {
		agentToolSessionID := d.sessions.CreateAgentToolSessionID(params.AgentMessageID, params.ToolCallID)
		session, err := d.sessions.CreateSubAgentSession(ctx, agentToolSessionID, params.SessionID, params.SessionTitle, params.AgentID)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("create session: %w", err)
		}
		sessionID = session.ID
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
	var runtime *streamRuntime
	if snap, ok := params.Agent.(interface {
		snapshotStreamRuntime(SessionAgentCall) streamRuntime
	}); ok {
		captured := snap.snapshotStreamRuntime(SessionAgentCall{SessionID: sessionID})
		runtime = &captured
	}

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

	priorMessages, err := d.carryOverMessages(ctx, budgetIn, params.SessionID, params.AgentID, sessionID)
	if err != nil {
		slog.Warn(
			"Failed to carry over sub-agent history; running without it",
			"agent", params.AgentID,
			"parent_session", params.SessionID,
			"child_session", sessionID,
			"error", err,
		)
	}

	// Call session setup function if provided
	if params.SessionSetup != nil {
		params.SessionSetup(sessionID)
	}

	// Get model configuration
	maxTokens := modelMaxOutputTokens(model)

	providerCfg, ok := d.cfg.Config().Providers.Get(model.ModelCfg.Provider)
	if !ok {
		return fantasy.ToolResponse{}, errModelProviderNotConfigured
	}

	// Run the agent. Takes its context explicitly - the non-detached path
	// below runs it with ctx directly, the detachable path with a child
	// context that can outlive ctx.
	run := func(runCtx context.Context) (*fantasy.AgentResult, error) {
		call := SessionAgentCall{
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
			// Sub-agents don't track an active runtime of their own, so
			// there's nothing for a refresh to update.
			OnAuthRefresh: d.authRefreshCallback(providerCfg),
		}
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
	// Legacy direct callers still own synchronous cost propagation. Task
	// launches provide ChildSessionID and are finalized atomically by the store.
	if params.ChildSessionID == "" {
		if costErr := d.updateParentSessionCost(context.WithoutCancel(ctx), sessionID, params.SessionID); costErr != nil {
			slog.Warn("Failed to update parent session cost", "child_session", sessionID, "parent_session", params.SessionID, "error", costErr)
		}
	}
	return d.finishSubAgent(subAgentOutcome{result: result, err: err}), nil
}

func (d *delegationFinalizer) subAgentTaskRun(parentSessionID, childSessionID, prompt string, agent SessionAgent, depth int) func(context.Context) (tools.TaskRunResult, error) {
	return func(ctx context.Context) (tools.TaskRunResult, error) {
		resp, err := d.runSubAgent(ctx, subAgentParams{
			Agent:          agent,
			SessionID:      parentSessionID,
			ChildSessionID: childSessionID,
			Prompt:         prompt,
			Depth:          depth,
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
// delegation is enabled.
func (d *delegationFinalizer) agentTool(_ context.Context, cfg agentConfig) (fantasy.AgentTool, error) {
	if _, ok := cfg.Agents()[config.AgentTask]; !ok {
		return nil, errors.New("task agent not configured")
	}
	return tools.WithToolSchemaConstraints(fantasy.NewParallelAgentTool(
		AgentToolName,
		agentToolDescription,
		func(ctx context.Context, params AgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			sessionID := tools.GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			return d.runBackgroundAgent(ctx, sessionID, params.Prompt, delegationSessionID(ctx, d.sessions, call.ID), delegationDepth(ctx))
		},
	), map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}), nil
}

//nolint:unparam // matches the (tool, error) signature of the other buildTools helpers
func (d *delegationFinalizer) agenticFetchTool(_ context.Context, client *http.Client) (fantasy.AgentTool, error) {
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = 100
		transport.MaxIdleConnsPerHost = 10
		transport.IdleConnTimeout = 90 * time.Second
		client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	}
	return tools.WithToolSchemaConstraints(fantasy.NewParallelAgentTool(
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
				SessionID:       d.sessions.CreateAgentToolSessionID(validation.AgentMessageID, call.ID),
				Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
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
					promptTemplate, err := prompt.NewPrompt("agentic_fetch", string(agenticFetchPromptTmpl), prompt.WithWorkingDir(tmpDir))
					if err != nil {
						return nil, cleanup, err
					}
					model, err := d.resolveAgentModel(ctx, true)
					if err != nil {
						return nil, cleanup, err
					}
					systemPrompt, err := promptTemplate.Build(ctx, model.Model.Provider(), model.Model.Model(), d.cfg)
					if err != nil {
						return nil, cleanup, err
					}
					providerCfg, ok := d.cfg.Config().Providers.Get(model.ModelCfg.Provider)
					if !ok {
						return nil, cleanup, errors.New("model provider not configured")
					}
					searchBackend, err := d.resolveWebSearchBackend()
					if err != nil {
						return nil, cleanup, fmt.Errorf("web_search: %w", err)
					}
					availability := tools.ResolveSystemToolAvailability()
					agent := NewSessionAgent(SessionAgentOptions{
						Model: model, SystemPromptPrefix: providerCfg.SystemPromptPrefix, SystemPrompt: systemPrompt,
						DisableAutoSummarize: d.cfg.Config().Options.DisableAutoSummarize,
						AutoSummarizeAt:      d.cfg.Config().Options.AutoSummarizeAt,
						Sessions:             d.sessions, Messages: d.messages,
						Tools: []fantasy.AgentTool{
							tools.NewWebFetchTool(nil, tmpDir, client, availability), tools.NewWebSearchTool(nil, tmpDir, client, searchBackend, availability),
							tools.NewGlobTool(tmpDir, d.cfg.Config().Tools.Glob), tools.NewSearchTool(tmpDir, d.cfg.Config().Tools.Grep),
							tools.NewReadTool(d.lspManager, d.permissions, d.filetracker, nil, tmpDir),
						},
					})
					return d.subAgentTaskRun(validation.SessionID, childID, fullPrompt, agent, childDepth), cleanup, nil
				},
			})
		},
	), map[string]tools.ToolSchemaConstraint{"prompt": {MinLength: intPointer(1)}}), nil
}

func (d *delegationFinalizer) customAgentTools(ctx context.Context, cfg agentConfig) ([]fantasy.AgentTool, error) {
	agents := cfg.Agents()
	ids := make([]string, 0, len(agents))
	for id := range agents {
		if id != config.AgentCoder && id != config.AgentTask {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	result := make([]fantasy.AgentTool, 0, len(ids))
	for _, id := range ids {
		tool, err := d.buildCustomAgentTool(ctx, id, agents[id])
		if err != nil {
			return nil, fmt.Errorf("build agent tool %q: %w", id, err)
		}
		result = append(result, tool)
	}
	return result, nil
}

//nolint:unparam // keeps the common tool-builder signature.
func (d *delegationFinalizer) buildCustomAgentTool(_ context.Context, id string, agentCfg config.Agent) (fantasy.AgentTool, error) {
	return fantasy.NewParallelAgentTool(
		id,
		customAgentDescription(id, agentCfg)+" The call returns immediately; correlate its later completion by task and child session id.",
		func(ctx context.Context, params CustomAgentParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			parentID := tools.GetSessionFromContext(ctx)
			if parentID == "" {
				return fantasy.ToolResponse{}, errors.New("session id missing from context")
			}
			childDepth := delegationDepth(ctx)
			latest, ok := d.cfg.Config().Agents[id]
			if !ok {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Agent %q is no longer configured.", id)), nil
			}
			return d.launchDelegation(ctx, tools.TaskCreateArgs{
				Goal:            params.Prompt,
				ParentSessionID: parentID,
				SessionTitle:    latest.Name,
				AgentID:         id,
				SessionID:       delegationSessionID(ctx, d.sessions, call.ID),
				Factory: func(ctx context.Context, childID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
					definition, ok := d.cfg.Config().Agents[id]
					if !ok {
						return nil, nil, fmt.Errorf("agent %q is no longer configured", id)
					}
					systemPrompt, err := prompt.NewPrompt(id, definition.Prompt, prompt.WithWorkingDir(d.cfg.WorkingDir()))
					if err != nil {
						return nil, nil, fmt.Errorf("parse prompt: %w", err)
					}
					agent, err := d.newSubAgent(ctx, systemPrompt, definition)
					if err != nil {
						return nil, nil, err
					}
					return d.subAgentTaskRun(parentID, childID, params.Prompt, agent, childDepth), nil, nil
				},
			})
		},
	), nil
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
