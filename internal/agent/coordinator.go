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
	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/agent/prompt"
	"github.com/rave-soft/braid/internal/agent/tools"
	"github.com/rave-soft/braid/internal/agent/tools/mcp"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/filetracker"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/hooks"
	"github.com/rave-soft/braid/internal/lsp"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/question"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/shell"
	"github.com/rave-soft/braid/internal/skills"
	"golang.org/x/sync/errgroup"

	"charm.land/fantasy/providers/openrouter"
)

// Coordinator errors.
var (
	errCoderAgentNotConfigured    = errors.New("coder agent not configured")
	errModelProviderNotConfigured = errors.New("model provider not configured")
	errModelNotSelected           = errors.New("model not selected")
	errModelNotFound              = errors.New("model not found in provider config")
	errBackgroundShellsRequired   = errors.New("background shell manager is required")
)

type Coordinator interface {
	// INFO: (kujtim) this is not used yet we will use this when we have multiple agents
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the backend.runAgent /
	// Coordinator / sessionAgent.Run layers: it reaches
	// sessionAgent.Run as SessionAgentCall.Accepted, where it is
	// consumed under dispatchMu once the accepted -> (cancel-on-entry |
	// queued | active) transition is chosen.
	RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// Steer passes call straight to the current agent's Steer, giving
	// callers an explicit "this is a steering follow-up" entry point
	// that reports whether the call was enqueued or ran. See
	// SessionAgent.Steer.
	Steer(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error)
	BeginAccepted(sessionID string) *AcceptedRun
	Cancel(sessionID string)
	CancelAll()
	IsSessionBusy(sessionID string) bool
	IsBusy() bool
	QueuedPrompts(sessionID string) int
	QueuedPromptsList(sessionID string) []string
	ClearQueue(sessionID string)
	Summarize(context.Context, string) error
	Model() Model
	UpdateModels(ctx context.Context) error
	GenerateTitle(ctx context.Context, sessionID, prompt string)
	// SetThreads wires (or clears, with nil) the thread manager the
	// thread_* tools are built against. It takes effect on the next
	// UpdateModels/buildTools pass, which every run performs, so callers
	// don't need to trigger a rebuild themselves. See buildTools: thread
	// tools are only ever offered to the top-level agent of the
	// workspace the manager belongs to, never to sub-agents.
	SetThreads(threads tools.ThreadManager)
	// SetTasks wires (or clears, with nil) the task manager the built-in
	// agent tool's background mode uses. Unlike SetThreads, this does not
	// change the tool list (the agent tool is always offered when
	// configured; only its background branch's availability depends on
	// this), so it takes effect immediately with no rebuild needed.
	SetTasks(tasks tools.TaskManager)
	// DeliverTaskCompletion enqueues completion into sessionID's
	// completion inbox for delivery on that session's next step (see
	// runTurn.prepareStep). internal/thread calls this once a task
	// reaches a terminal status, having resolved sessionID as the
	// task's *parent* session - never the task's own child session.
	DeliverTaskCompletion(sessionID string, completion TaskCompletion)
	// RefreshSkills replaces the coordinator's cached skill discovery
	// results — called by the backend after its skills-directory watcher
	// detects a SKILL.md added, edited, or removed outside this process,
	// so a hot-reload takes effect on the next Run without a restart. It
	// preserves the skill tracker's loaded-state for names that are still
	// active rather than resetting it, so a skill already read earlier in
	// the session does not appear to forget itself.
	RefreshSkills(allSkills, activeSkills []*skills.Skill)
}

type coordinator struct {
	cfg         *config.ConfigStore
	sessions    session.Service
	messages    message.Service
	permissions permission.Service
	questions   question.Service
	history     history.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	interactive bool
	mcp         *mcp.Registry
	background  *shell.BackgroundShellManager

	localVersion atomic.Uint64
	runtime      *runtimeCache
	// toolsCache remains a compatibility alias for coordinators assembled by
	// focused tests; runtime owns the actual compiled cache.
	toolsCache *runtimeCache

	// threadsMu guards threads, which SetThreads may set after
	// construction (thread managers are wired in post-bootstrap; see
	// internal/cmd/root.go and internal/backend/backend.go) and buildTools
	// reads on every run via UpdateModels.
	threadsMu sync.RWMutex
	threads   tools.ThreadManager

	// tasksMu guards tasks, wired the same way and for the same reason as
	// threads above, but read by the "agent" tool's background branch at
	// call time rather than by buildTools — see SetTasks's doc comment on
	// the Coordinator interface for why no rebuild is needed.
	tasksMu sync.RWMutex
	tasks   tools.TaskManager

	currentAgent SessionAgent
	agents       map[string]SessionAgent

	// skillsMu guards allSkills/activeSkills/skillTracker. They start as a
	// session-start snapshot, but RefreshSkills (called from the backend's
	// skills-directory watcher goroutine) can replace them mid-session
	// while a Run is concurrently reading them via buildTools/
	// logTurnSkillUsage — a plain field would race those reads. The
	// skillTracker pointer itself is not replaced (see RefreshSkills), so
	// its own internal lock is what protects loaded/activeNames; this
	// mutex only protects which *slices*/tracker the coordinator hands out.
	skillsMu     sync.RWMutex
	allSkills    []*skills.Skill // Pre-filter: all discovered after dedup.
	activeSkills []*skills.Skill // Post-filter: active skills only.
	skillTracker *skills.Tracker

	readyWg errgroup.Group

	// lifecycleOnce/lifecycleCtx/lifecycleCancel bound buildAgent's
	// background readiness work (see buildAgent) to the coordinator's own
	// lifetime rather than to whichever caller context triggered the
	// build. Lazily created via ensureLifecycle rather than in
	// NewCoordinator alone, since some tests build a coordinator as a bare
	// struct literal (bypassing NewCoordinator) and still call buildAgent
	// directly.
	lifecycleOnce   sync.Once
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc

	// readyMu guards readyGroup.Add against Close's readyGroup.Wait:
	// sync.WaitGroup forbids a Wait racing an Add that could hand the
	// counter a positive value after Wait has already observed (or is
	// about to observe) zero. Every readyGroup.Add(2) in buildAgent, and
	// the closing flag it's conditioned on, happen under this lock; Close
	// takes the lock only to flip closing, then calls Wait unlocked (Wait
	// itself must not run under the lock, or a concurrent buildAgent call
	// would deadlock on the lock instead of just skipping its Add).
	readyMu sync.Mutex
	closing bool

	// readyGroup counts every outstanding readiness goroutine buildAgent
	// has started (main agent and every sub-agent rebuild), regardless of
	// which errgroup they report through. Close waits on it.
	readyGroup sync.WaitGroup

	// closeOnce/closeDone make Close idempotent: readyGroup.Wait must run
	// at most once ever (a second concurrent Wait call is itself the
	// "WaitGroup reused before previous Wait returned" panic, independent
	// of the readyMu protection above), so only the first Close call
	// starts it; every call, including the first, waits on closeDone.
	closeOnce sync.Once
	closeDone chan struct{}
}

// ensureLifecycle lazily creates the coordinator's lifecycle context on
// first use and returns it. Safe for concurrent callers.
func (c *coordinator) ensureLifecycle() context.Context {
	c.lifecycleOnce.Do(func() {
		c.lifecycleCtx, c.lifecycleCancel = context.WithCancel(context.Background())
	})
	return c.lifecycleCtx
}

// Close cancels the coordinator's background readiness work (buildAgent's
// async system-prompt/tool-list setup, including sub-agent rebuilds on
// every run) and waits for it to actually stop, bounded by ctx. This is
// what keeps the git/MCP subprocesses that work may spawn (see
// internal/agent/prompt) from outliving the coordinator — production
// callers wire it into App.Shutdown; tests that build a coordinator
// directly should call it in their own teardown for the same reason.
// Safe to call even if buildAgent was never invoked, and safe to call
// concurrently or more than once: every call waits on the same
// closeDone, but only the first ever starts readyGroup.Wait.
func (c *coordinator) Close(ctx context.Context) error {
	c.ensureLifecycle()

	c.closeOnce.Do(func() {
		c.closeDone = make(chan struct{})

		// closing must be set, and observed by every future buildAgent
		// call, before readyGroup.Wait starts: once Wait is running, no
		// further Add is allowed, so no further readiness goroutine may
		// start either. Cancel lifecycleCtx after releasing readyMu —
		// it only unblocks already-running goroutines (which reduce
		// readyGroup, not add to it), so it doesn't need the lock.
		c.readyMu.Lock()
		c.closing = true
		c.readyMu.Unlock()
		c.lifecycleCancel()

		go func() {
			c.readyGroup.Wait()
			close(c.closeDone)
		}()
	})

	select {
	case <-c.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CoordinatorOptions holds the dependencies for NewCoordinator. Using a
// struct keeps the constructor self-documenting and avoids a long
// positional parameter list.
type CoordinatorOptions struct {
	Config      *config.ConfigStore
	Sessions    session.Service
	Messages    message.Service
	Permissions permission.Service
	Questions   question.Service
	History     history.Service
	FileTracker filetracker.Service
	LSPManager  *lsp.Manager
	Notify      pubsub.Publisher[notify.Notification]
	RunComplete pubsub.Publisher[notify.RunComplete]
	Skills      *skills.Manager
	Interactive bool
	// MCP is the per-workspace MCP registry. Every consumer that used to
	// reach for the mcp package's shared defaultRegistry now goes through
	// this instance so two workspaces in one process don't share sessions,
	// states, or auth handlers keyed by MCP server name. See
	// ARCHITECTURE_REVIEW.md section 3.1.
	MCP *mcp.Registry
	// Threads is nil-safe: when nil, the thread_* tools are simply
	// omitted from the top-level agent's tool list. It is normally wired
	// after construction via [Coordinator.SetThreads] instead of here,
	// since the thread manager is set up post-bootstrap; this field
	// exists mainly so tests and other in-process callers can supply one
	// up front.
	Threads tools.ThreadManager
	// Tasks is nil-safe the same way Threads is: when nil, the built-in
	// agent tool's background branch reports background delegation as
	// unavailable rather than silently running in the foreground. Wired
	// after construction via [Coordinator.SetTasks] in production, same
	// as Threads.
	Tasks            tools.TaskManager
	BackgroundShells *shell.BackgroundShellManager
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	if opts.BackgroundShells == nil {
		return nil, errBackgroundShellsRequired
	}

	// Skills are pre-discovered by the caller (see app.New /
	// backend.CreateWorkspace) and passed in via the manager. If no
	// manager was provided (legacy callers), fall back to an in-line
	// discovery so the coordinator still works.
	var allSkills, activeSkills []*skills.Skill
	if opts.Skills != nil {
		allSkills = opts.Skills.AllSkills()
		activeSkills = opts.Skills.ActiveSkills()
	} else {
		allSkills, activeSkills = discoverSkills(opts.Config)
	}
	skillTracker := skills.NewTracker(activeSkills)

	c := &coordinator{
		cfg:          opts.Config,
		sessions:     opts.Sessions,
		messages:     opts.Messages,
		permissions:  opts.Permissions,
		questions:    opts.Questions,
		history:      opts.History,
		filetracker:  opts.FileTracker,
		lspManager:   opts.LSPManager,
		notify:       opts.Notify,
		runComplete:  opts.RunComplete,
		agents:       make(map[string]SessionAgent),
		allSkills:    allSkills,
		activeSkills: activeSkills,
		skillTracker: skillTracker,
		interactive:  opts.Interactive,
		mcp:          opts.MCP,
		threads:      opts.Threads,
		tasks:        opts.Tasks,
		background:   opts.BackgroundShells,
		runtime:      newRuntimeCache(),
	}

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.currentAgent = agent
	c.agents[config.AgentCoder] = agent
	return c, nil
}

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.run(ctx, accept, sessionID, prompt, attachments...)
}

// run is the shared implementation behind Run and RunAccepted. When
// accept is non-nil it is threaded onto the SessionAgentCall as
// Accepted so sessionAgent.Run can consume the accept reservation under
// dispatchMu; when nil (the in-process/local path) no accept tracking
// applies.
func (c *coordinator) run(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	// Wait for MCP initialization to complete before building the tool list.
	// Without this, slow-to-start MCP servers (e.g. stdio Python via uv) may
	// not have registered their tools yet when buildTools reads the registry,
	// so their tools silently never appear in the LLM tool palette — even
	// though braid_info reports them as connected.
	if err := c.waitForMCPInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	runtime, err := c.runtimeFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare agent runtime: %w", err)
	}

	if err := c.refreshTokenIfExpired(ctx, runtime.providerCfg); err != nil {
		// NOTE(@andreynering): We don't return here because the event handling to ask the user to reauthenticate
		// depends on the flow below. If refresh fails, proceed with the token we have.
		slog.Error("Failed to refresh OAuth2 token. Proceeding with existing token.", "error", err)
	} else if c.runtimeKey() != runtime.key {
		runtime, err = c.runtimeFor(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to prepare refreshed agent runtime: %w", err)
		}
	}
	active := newActiveRuntime(runtime)

	// Coalesce per-attempt RunComplete payloads so only the final
	// outcome reaches subscribers. Without this, the first attempt's
	// failed RunComplete (unauthorized) would race ahead of the
	// retry's success, and `braid run` would exit on the stale error
	// before ever seeing the retry result. Each attempt's
	// SessionAgentCall.OnComplete hook overwrites latest; we publish
	// exactly once after retries resolve, via PublishMustDeliver, so
	// a momentarily-full subscriber buffer can't silently drop the
	// terminal event.
	var (
		latest    notify.RunComplete
		hasLatest bool
	)
	onComplete := func(rc notify.RunComplete) {
		latest = rc
		hasLatest = true
	}
	// Propagate the caller-supplied RunID (set via agent.WithRunID
	// at the HTTP boundary in backend.SendMessage) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
			Runtime:          runtime,
			ActiveRuntime:    active,
			SessionID:        sessionID,
			RunID:            runID,
			Prompt:           prompt,
			Attachments:      attachments,
			MaxOutputTokens:  runtime.maxOutputTokens,
			ProviderOptions:  runtime.providerOptions,
			Temperature:      runtime.temperature,
			TopP:             runtime.topP,
			TopK:             runtime.topK,
			FrequencyPenalty: runtime.frequencyPenalty,
			PresencePenalty:  runtime.presencePenalty,
			OnComplete:       onComplete,
			Accepted:         accept,
			OnAuthRefresh:    c.makeAuthRefreshCallback(runtime.providerCfg, active),
		})
	}
	_, activeSkillsSnapshot, skillTrackerSnapshot := c.skillsSnapshot()
	beforeLoaded := skillTrackerSnapshot.LoadedNames()
	result, originalErr := run()
	logTurnSkillUsage(sessionID, prompt, activeSkillsSnapshot, skillTrackerSnapshot, beforeLoaded)

	// Notify only if still unauthorized after retry — a successful
	// retry means the user doesn't need to re-authenticate. AWS SSO is
	// handled transparently inside OnAuthRefresh, so it needs no post-run
	// notification here.
	if hasLatest && c.runComplete != nil {
		c.runComplete.PublishMustDeliver(ctx, pubsub.UpdatedEvent, latest)
		// Signal to the dispatcher (backend.runAgent) that the
		// authoritative terminal RunComplete for this run was already
		// emitted, so it does not publish a duplicate fallback for the
		// error it is about to receive.
		MarkRunCompletePublished(ctx)
	}
	return result, originalErr
}

// waitForMCPInit blocks until this coordinator's MCP registry finishes
// initializing. A coordinator built without a registry (a handful of
// tests construct one directly) has nothing to wait for.
func (c *coordinator) waitForMCPInit(ctx context.Context) error {
	if c.mcp == nil {
		return nil
	}
	return c.mcp.WaitForInit(ctx)
}

func mergeCallOptions(model Model, cfg config.ProviderConfig) (fantasy.ProviderOptions, *float64, *float64, *int64, *float64, *float64) {
	modelOptions := getProviderOptions(model, cfg)
	temp := cmp.Or(model.ModelCfg.Temperature, model.CatalogCfg.Options.Temperature)
	topP := cmp.Or(model.ModelCfg.TopP, model.CatalogCfg.Options.TopP)
	topK := cmp.Or(model.ModelCfg.TopK, model.CatalogCfg.Options.TopK)
	freqPenalty := cmp.Or(model.ModelCfg.FrequencyPenalty, model.CatalogCfg.Options.FrequencyPenalty)
	presPenalty := cmp.Or(model.ModelCfg.PresencePenalty, model.CatalogCfg.Options.PresencePenalty)
	return modelOptions, temp, topP, topK, freqPenalty, presPenalty
}

func (c *coordinator) buildAgent(ctx context.Context, prompt *prompt.Prompt, agent config.Agent, isSubAgent bool) (SessionAgent, error) {
	model, err := c.buildAgentModel(ctx, isSubAgent)
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
		primary, err = c.buildCustomAgentModel(ctx, agent, isSubAgent)
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

	providerCfg, _ := c.cfg.Config().Providers.Get(primary.ModelCfg.Provider)
	result := NewSessionAgent(SessionAgentOptions{
		Model:                primary,
		SystemPromptPrefix:   providerCfg.SystemPromptPrefix,
		SystemPrompt:         "",
		IsSubAgent:           isSubAgent,
		DisableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		MCP:                  c.mcp,
	})

	// The readiness goroutines below perform one-time setup — building the
	// system prompt and the (MCP-gated) tool list — whose results the
	// coordinator needs for its whole lifetime, so they must survive the
	// caller's context being canceled. Several entry points build an agent
	// from a short-lived HTTP request context: the server's
	// InitAgent/UpdateAgent handlers, and UpdateModels -> buildTools ->
	// agentTool -> buildAgent for the sub-agent. Because mcp.WaitForInit
	// blocks until MCP initialization finishes, a slow MCP server keeps one
	// of these goroutines parked past the request; if it inherited the
	// caller's cancellation, that request context going away (or, for the
	// sub-agent rebuilt on every run, the *next* run's context replacing
	// this one) would abort the work before emitting anything — the
	// client/server session hangs with no visible response. c.lifecycleCtx
	// (see ensureLifecycle) is scoped to the coordinator itself instead: it
	// only cancels when the coordinator is Close()d, so the work keeps
	// running until it either finishes or the coordinator shuts down —
	// bounding the git/MCP subprocesses it may spawn (see
	// internal/agent/prompt) to the coordinator's lifetime instead of
	// leaking them past it (e.g. past a test's t.TempDir cleanup).
	initCtx := c.ensureLifecycle()

	// ready is the errgroup these two goroutines report through.
	// c.readyWg is reserved for the coordinator's own top-level agent,
	// built exactly once in NewCoordinator: run()'s preamble waits on it
	// before the first turn. Sub-agents (isSubAgent=true), by contrast,
	// are rebuilt from scratch on every UpdateModels -> buildTools ->
	// agentTool pass, i.e. on every coordinator.Run call. Sharing
	// c.readyWg across those rebuilds raced sync.WaitGroup's Add against
	// a concurrent run's Wait once threads started dispatching multiple
	// Run calls at once on the same coordinator (Manager.dispatch, fired
	// from both Create and Send with no coordinator-level serialization),
	// and — independent of concurrency — errgroup.Group only remembers
	// its first error, so one failed sub-agent rebuild would permanently
	// fail readyWg.Wait() for every later run. A local group per build
	// keeps each sub-agent's readiness self-contained; nothing needs to
	// block on it, so failures are logged instead of propagated.
	ready := &c.readyWg
	if isSubAgent {
		ready = &errgroup.Group{}
	}

	// c.readyGroup tracks every readiness goroutine buildAgent ever
	// starts (main agent and every sub-agent rebuild alike), independent
	// of which errgroup above they report through. Close waits on it —
	// bounded by the ctx it's given — so it can hand back control once
	// this work has actually stopped touching the filesystem/network,
	// rather than merely having asked it to via lifecycleCtx.
	//
	// The Add must happen under readyMu, gated on !closing: once Close
	// has flipped closing and started readyGroup.Wait, an Add from here
	// racing that Wait is exactly the "WaitGroup reused before previous
	// Wait returned" panic — Wait can observe the counter at zero between
	// two already-running goroutines' Done calls, at which point a new
	// Add must not be allowed to resurrect it. When closing, the
	// coordinator is shutting down and nothing will use this agent's
	// system prompt/tools again, so skip the readiness work entirely
	// rather than starting goroutines Close can never safely wait for.
	c.readyMu.Lock()
	if c.closing {
		c.readyMu.Unlock()
		return result, nil
	}
	c.readyGroup.Add(2)
	c.readyMu.Unlock()

	ready.Go(func() error {
		defer c.readyGroup.Done()
		systemPrompt, err := prompt.Build(initCtx, primary.Model.Provider(), primary.Model.Model(), c.cfg)
		if err != nil {
			return err
		}
		result.SetSystemPrompt(systemPrompt)
		return nil
	})

	ready.Go(func() error {
		defer c.readyGroup.Done()
		// Wait for MCP servers to finish registering their tools before
		// building the initial tool list. This ensures the first tool set
		// (used if anything reads it before run() rebuilds) includes all
		// MCP tools, not just fast-to-init ones.
		if err := c.waitForMCPInit(initCtx); err != nil {
			return err
		}
		tools, err := c.buildTools(initCtx, agent, isSubAgent)
		if err != nil {
			return err
		}
		result.SetTools(tools)
		return nil
	})

	if isSubAgent {
		// Nothing blocks on a sub-agent's readiness today (the delegation
		// tool isn't invoked until much later in the conversation, by
		// which point this has long finished), so just surface failures
		// instead of silently dropping them.
		go func() {
			if err := ready.Wait(); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("Failed to prepare sub-agent", "agent", agent.Name, "error", err)
			}
		}()
	}

	return result, nil
}

func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	var allTools []fantasy.AgentTool
	if slices.Contains(agent.AllowedTools, AgentToolName) {
		agentTool, err := c.agentTool(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agentTool)
	}

	// User-defined agents are offered to the top-level agent only. Handing
	// them to a sub-agent would let delegation nest without bound and would
	// recurse here at build time, since building a delegation tool builds the
	// target agent, which builds its own tool list.
	if !isSubAgent {
		customTools, err := c.customAgentTools(ctx)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, customTools...)
	}

	if slices.Contains(agent.AllowedTools, tools.AgenticFetchToolName) {
		agenticFetchTool, err := c.agenticFetchTool(ctx, nil)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, agenticFetchTool)
	}

	// Get the model name for the agent.
	modelID := ""
	if modelCfg := c.cfg.Config().Model; modelCfg.Model != "" {
		if model := c.cfg.Config().GetModel(modelCfg.Provider, modelCfg.Model); model != nil {
			modelID = model.ID
		}
	}

	logFile := config.GlobalLogFile()

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := c.cfg.Config().Hooks[hooks.EventPreToolUse]; len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
	}

	searchBackend, err := c.webSearchBackend()
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}

	allSkillsSnapshot, activeSkillsSnapshot, skillTrackerSnapshot := c.skillsSnapshot()

	allTools = append(
		allTools,
		tools.NewBashTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Options.Attribution, modelID, c.background),
		tools.NewBraidInfoTool(c.cfg, c.mcp, c.lspManager, allSkillsSnapshot, activeSkillsSnapshot, skillTrackerSnapshot),
		tools.NewBraidLogsTool(logFile),
		tools.NewJobOutputTool(c.background),
		tools.NewJobKillTool(c.background),
		tools.NewDownloadTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewMultiEditTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
		tools.NewFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewWebFetchTool(c.permissions, c.cfg.WorkingDir(), nil),
		tools.NewWebSearchTool(c.permissions, c.cfg.WorkingDir(), nil, searchBackend),
		tools.NewGlobTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Glob),
		tools.NewSearchTool(c.cfg.WorkingDir(), c.cfg.Config().Tools.Grep),
		tools.NewLsTool(c.permissions, c.cfg.WorkingDir(), c.cfg.Config().Tools.Ls),
		tools.NewTodosTool(c.sessions),
		tools.NewViewTool(c.lspManager, c.permissions, c.filetracker, skillTrackerSnapshot, c.cfg.WorkingDir(), c.cfg.Config().Options.SkillsPaths...),
		tools.NewWriteTool(c.lspManager, c.permissions, c.history, c.filetracker, c.cfg.WorkingDir()),
	)

	// Thread tools manage parallel agent work streams in their own git
	// worktrees. Offered only to the top-level agent of the workspace
	// that owns the thread manager: sub-agents never get them (spawning
	// threads from a delegated task would nest workspace ownership in a
	// way the manager doesn't support), and there simply is no manager
	// for non-git or thread-spawned workspaces (see internal/cmd/root.go
	// and internal/backend/backend.go).
	if !isSubAgent {
		if threads := c.threadsManager(); threads != nil {
			allTools = append(
				allTools,
				tools.NewThreadCreateTool(threads, c.permissions),
				tools.NewThreadListTool(threads),
				tools.NewThreadStatusTool(threads),
				tools.NewThreadSendTool(threads),
				tools.NewThreadWaitTool(threads),
				tools.NewThreadMergeTool(threads, c.permissions),
				tools.NewThreadRemoveTool(threads, c.permissions),
			)
		}
	}

	// Task tools observe and steer background task delegations (see the
	// "agent" tool's background mode, which creates them). Same
	// restriction as thread tools, for the same reason: only the
	// top-level agent of the workspace that owns the task manager gets
	// them, and there is no manager for a workspace that doesn't own one.
	if !isSubAgent {
		if taskManager := c.tasksManager(); taskManager != nil {
			allTools = append(
				allTools,
				tools.NewTaskListTool(taskManager),
				tools.NewTaskResultTool(taskManager),
				tools.NewTaskCancelTool(taskManager, c.permissions),
				tools.NewTaskSendTool(taskManager),
				tools.NewTaskOutputTool(taskManager),
			)
		}
	}

	// Question tool is interactive-only and not available to sub-agents.
	if !isSubAgent && c.interactive {
		allTools = append(allTools, tools.NewQuestionTool(c.questions))
	}

	// Add LSP tools if user has configured LSPs or auto_lsp is enabled (nil or true).
	if len(c.cfg.Config().LSP) > 0 || c.cfg.Config().Options.AutoLSP == nil || *c.cfg.Config().Options.AutoLSP {
		allTools = append(
			allTools,
			tools.NewDiagnosticsTool(c.lspManager),
			tools.NewReferencesTool(c.lspManager),
			tools.NewLSPRestartTool(c.lspManager),
			tools.NewSymbolsTool(c.lspManager),
			tools.NewDefinitionTool(c.lspManager),
			tools.NewCallHierarchyTool(c.lspManager),
			tools.NewRenameTool(c.lspManager, c.permissions, c.history, c.filetracker),
			tools.NewReplaceSymbolTool(c.lspManager, c.permissions, c.history, c.filetracker),
		)
	}

	if len(c.cfg.Config().MCP) > 0 {
		allTools = append(
			allTools,
			tools.NewListMCPResourcesTool(c.cfg, c.mcp, c.permissions),
			tools.NewReadMCPResourceTool(c.cfg, c.mcp, c.permissions),
		)
	}

	// grep and ripgrep are alternative registrations of the same content
	// search slot (which one exists depends on whether rg is installed), so
	// an agent allowing either name gets whichever is available.
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

	for _, tool := range tools.GetMCPTools(c.mcp, c.permissions, c.cfg, c.cfg.WorkingDir()) {
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

		for mcp, tools := range agent.AllowedMCP {
			if mcp != tool.MCP() {
				continue
			}
			if len(tools) == 0 || slices.Contains(tools, tool.MCPToolName()) {
				filteredTools = append(filteredTools, tool)
				break
			}
			slog.Debug("MCP not allowed", "tool", tool.Name(), "agent", agent.Name)
		}
	}
	slices.SortFunc(filteredTools, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	// Wrap tools with hook interception for the top-level agent only.
	// Sub-agents (the `agent` task tool, `agentic_fetch`, etc.) run
	// without hook interception to avoid firing the user's hook N times
	// per delegated turn. The top-level invocation of the sub-agent tool
	// itself is still wrapped from the coder's side.
	filteredTools = wrapToolsWithHooks(filteredTools, hookRunner, isSubAgent)

	return filteredTools, nil
}

// webSearchBackend builds the SearchBackend selected by options.web_search,
// defaulting to the keyless DuckDuckGo scraper when the section is absent.
// api_key and proxy_url run through the same shell-expansion resolver used
// for provider api_key/proxy_url.
func (c *coordinator) webSearchBackend() (tools.SearchBackend, error) {
	var opts config.WebSearchOptions
	if ws := c.cfg.Config().Options.WebSearch; ws != nil {
		opts = *ws
	}
	return tools.NewSearchBackend(opts, c.cfg.Resolver(), nil)
}

// TODO: when we support multiple agents we need to change this so that we pass in the agent specific model config
func (c *coordinator) buildAgentModel(ctx context.Context, isSubAgent bool) (Model, error) {
	modelCfg := c.cfg.Config().Model
	if modelCfg.Model == "" {
		return Model{}, errModelNotSelected
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(modelCfg.Provider)
	if !ok {
		return Model{}, errModelProviderNotConfigured
	}

	provider, err := c.buildProvider(providerCfg, modelCfg, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	var catalogModel *catwalk.Model
	for _, m := range providerCfg.Models {
		if m.ID == modelCfg.Model {
			catalogModel = &m
		}
	}
	if catalogModel == nil {
		return Model{}, errModelNotFound
	}

	modelID := modelCfg.Model
	if modelCfg.Provider == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	return Model{
		Model:      languageModel,
		CatalogCfg: *catalogModel,
		ModelCfg:   modelCfg,
		FlatRate:   providerCfg.FlatRate,
	}, nil
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
func (c *coordinator) buildCustomAgentModel(ctx context.Context, agent config.Agent, isSubAgent bool) (Model, error) {
	match, err := config.ResolveModelString(c.cfg.Config().Providers.Copy(), agent.Model)
	if err != nil {
		return Model{}, fmt.Errorf("agent %q model %q: %w", agent.Name, agent.Model, err)
	}

	providerCfg, ok := c.cfg.Config().Providers.Get(match.Provider)
	if !ok {
		return Model{}, fmt.Errorf("agent %q model %q: provider %q not configured", agent.Name, agent.Model, match.Provider)
	}

	selected := config.SelectedModel{Provider: match.Provider, Model: match.ModelID}

	provider, err := c.buildProvider(providerCfg, selected, isSubAgent)
	if err != nil {
		return Model{}, err
	}

	var catwalkModel *catwalk.Model
	for _, m := range providerCfg.Models {
		if m.ID == match.ModelID {
			catwalkModel = &m
			break
		}
	}
	if catwalkModel == nil {
		return Model{}, fmt.Errorf("agent %q model %q: model not found in provider config", agent.Name, agent.Model)
	}

	modelID := match.ModelID
	if match.Provider == openrouter.Name && isExactoSupported(modelID) {
		modelID += ":exacto"
	}

	languageModel, err := provider.LanguageModel(ctx, modelID)
	if err != nil {
		return Model{}, err
	}

	return Model{
		Model:      languageModel,
		CatalogCfg: *catwalkModel,
		ModelCfg:   selected,
		FlatRate:   providerCfg.FlatRate,
	}, nil
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

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.currentAgent.BeginAccepted(sessionID)
}

// Steer implements Coordinator.
func (c *coordinator) Steer(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error) {
	return c.currentAgent.Steer(ctx, call)
}

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
}

func (c *coordinator) CancelAll() {
	c.currentAgent.CancelAll()
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

// SetThreads implements Coordinator.
func (c *coordinator) SetThreads(threads tools.ThreadManager) {
	c.threadsMu.Lock()
	c.threads = threads
	c.threadsMu.Unlock()
	c.invalidateRuntime()
}

// SetTasks implements Coordinator. No invalidateRuntime call: unlike
// SetThreads, this never changes which tools are offered (the agent tool
// is always present when configured), so there is no cached tool list to
// rebuild — tasksManager is read fresh on every background call instead.
func (c *coordinator) SetTasks(tasks tools.TaskManager) {
	c.tasksMu.Lock()
	c.tasks = tasks
	c.tasksMu.Unlock()
}

// DeliverTaskCompletion implements Coordinator.
func (c *coordinator) DeliverTaskCompletion(sessionID string, completion TaskCompletion) {
	c.currentAgent.DeliverTaskCompletion(sessionID, completion)
}

// RefreshSkills implements Coordinator.RefreshSkills.
func (c *coordinator) RefreshSkills(allSkills, activeSkills []*skills.Skill) {
	c.skillsMu.Lock()
	c.allSkills = allSkills
	c.activeSkills = activeSkills
	// The tracker itself is not replaced: UpdateActiveSkills mutates it
	// in place under its own lock, keeping loaded state for names still
	// active rather than wiping it (see UpdateActiveSkills).
	tracker := c.skillTracker
	c.skillsMu.Unlock()
	tracker.UpdateActiveSkills(activeSkills)
	c.invalidateRuntime()
}

// skillsSnapshot returns the coordinator's current skill discovery
// results under skillsMu, for callers (buildTools, Run) that need a
// consistent read while RefreshSkills may be running concurrently.
func (c *coordinator) skillsSnapshot() (allSkills, activeSkills []*skills.Skill, tracker *skills.Tracker) {
	c.skillsMu.RLock()
	defer c.skillsMu.RUnlock()
	return c.allSkills, c.activeSkills, c.skillTracker
}

// threadsManager returns the currently wired thread manager, or nil.
func (c *coordinator) threadsManager() tools.ThreadManager {
	c.threadsMu.RLock()
	defer c.threadsMu.RUnlock()
	return c.threads
}

// tasksManager returns the currently wired task manager, or nil.
func (c *coordinator) tasksManager() tools.TaskManager {
	c.tasksMu.RLock()
	defer c.tasksMu.RUnlock()
	return c.tasks
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return c.currentAgent.IsSessionBusy(sessionID)
}

func (c *coordinator) Model() Model {
	return c.currentAgent.Model()
}

func (c *coordinator) runtimeKey() runtimeKey {
	key := runtimeKey{local: c.localVersion.Load()}
	if c.cfg != nil {
		key.config = c.cfg.Version()
	}
	if c.mcp != nil {
		key.mcp = c.mcp.Version()
	}
	return key
}

func (c *coordinator) invalidateRuntime() {
	c.localVersion.Add(1)
	if c.runtime != nil {
		c.runtime.invalidate()
	}
}

func (c *coordinator) runtimeFor(ctx context.Context) (*compiledRuntime, error) {
	if c.runtime == nil {
		c.runtime = c.toolsCache
		if c.runtime == nil {
			c.runtime = newRuntimeCache()
		}
	}
	return c.runtime.getOrBuild(ctx, c.runtimeKey, func(ctx context.Context, key runtimeKey) (*compiledRuntime, error) {
		model, err := c.buildAgentModel(ctx, false)
		if err != nil {
			return nil, err
		}
		agentCfg, ok := c.cfg.Config().Agents[config.AgentCoder]
		if !ok {
			return nil, errCoderAgentNotConfigured
		}
		builtTools, err := c.buildTools(ctx, agentCfg, false)
		if err != nil {
			return nil, err
		}
		runtimePrompt, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
		if err != nil {
			return nil, err
		}
		systemPrompt, err := runtimePrompt.Build(ctx, model.Model.Provider(), model.Model.Model(), c.cfg)
		if err != nil {
			return nil, err
		}
		if len(builtTools) > 0 {
			builtTools[len(builtTools)-1].SetProviderOptions(cacheControlOptions())
		}
		providerCfg, ok := c.cfg.Config().Providers.Get(model.ModelCfg.Provider)
		if !ok {
			return nil, errModelProviderNotConfigured
		}
		options, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(model, providerCfg)
		maxTokens := model.CatalogCfg.DefaultMaxTokens
		if model.ModelCfg.MaxTokens != 0 {
			maxTokens = model.ModelCfg.MaxTokens
		}
		return &compiledRuntime{
			key: key, model: model, tools: builtTools, systemPrompt: systemPrompt,
			providerCfg: providerCfg, providerOptions: options,
			temperature: temp, topP: topP, topK: topK,
			frequencyPenalty: freqPenalty, presencePenalty: presPenalty,
			maxOutputTokens:      maxTokens,
			systemPromptPrefix:   providerCfg.SystemPromptPrefix,
			disableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
		}, nil
	})
}

func (c *coordinator) UpdateModels(ctx context.Context) error {
	runtime, err := c.runtimeFor(ctx)
	if errors.Is(err, errRuntimeChanged) {
		runtime, err = c.runtimeFor(ctx)
	}
	if err != nil {
		return err
	}
	c.currentAgent.SetModel(runtime.model)
	c.currentAgent.SetTools(runtime.tools)
	c.currentAgent.SetSystemPrompt(runtime.systemPrompt)
	return nil
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	runtime, err := c.runtimeFor(ctx)
	if err != nil {
		return err
	}
	if err := c.refreshTokenIfExpired(ctx, runtime.providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	} else if c.runtimeKey() != runtime.key {
		runtime, err = c.runtimeFor(ctx)
		if err != nil {
			return err
		}
	}
	active := newActiveRuntime(runtime)

	if agent, ok := c.currentAgent.(*sessionAgent); ok {
		return agent.summarize(ctx, sessionID, runtime.providerOptions, c.makeAuthRefreshCallback(runtime.providerCfg, active), runtime.model, runtime.systemPromptPrefix, active)
	}
	return c.currentAgent.Summarize(ctx, sessionID, runtime.providerOptions, c.makeAuthRefreshCallback(runtime.providerCfg, active))
}

// GenerateTitle generates a session title using the current agent.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	if c.currentAgent == nil {
		return
	}
	runtime, err := c.runtimeFor(ctx)
	if err != nil {
		slog.Error("Failed to prepare agent runtime for title", "error", err)
		return
	}
	if agent, ok := c.currentAgent.(*sessionAgent); ok {
		agent.generateTitle(ctx, sessionID, prompt, runtime.model, runtime.systemPromptPrefix)
		return
	}
	c.currentAgent.GenerateTitle(ctx, sessionID, prompt)
}

// discoverSkills is a thin fallback wrapper used only when no
// skills.Manager has been threaded through to the coordinator. All
// production call sites (backend.CreateWorkspace, setupLocalWorkspace)
// run discovery in advance and pass the results via the manager;
// reaching this path means a caller bypassed both. It deliberately does
// NOT publish to the package-level broker — there are no subscribers in
// that case, so doing so would be misleading without delivering the
// snapshot anywhere useful.
func discoverSkills(cfg *config.ConfigStore) (allSkills, activeSkills []*skills.Skill) {
	opts := cfg.Config().Options
	var paths, disabled []string
	if opts != nil {
		paths = opts.SkillsPaths
		disabled = opts.DisabledSkills
	}
	var resolver func(string) (string, error)
	if r := cfg.Resolver(); r != nil {
		resolver = r.ResolveValue
	}
	allSkills, activeSkills, states := skills.DiscoverFromConfig(skills.DiscoveryConfig{
		SkillsPaths:    paths,
		DisabledSkills: disabled,
		Resolver:       resolver,
	})
	logDiscoveryStats(states, paths, allSkills, activeSkills, disabled)
	return allSkills, activeSkills
}

// logTurnSkillUsage emits a per-turn diagnostic line showing which skills
// (if any) were loaded during this turn and which looked relevant based on
// a cheap keyword match against the user prompt. The goal is to surface
// "should-have-loaded but didn't" situations for later analysis.
//
// Logged at Info level under component=skills; heavy fields are elided when
// there is nothing interesting to report.
func logTurnSkillUsage(
	sessionID string,
	prompt string,
	activeSkills []*skills.Skill,
	tracker *skills.Tracker,
	before []string,
) {
	if tracker == nil || len(activeSkills) == 0 {
		return
	}

	after := tracker.LoadedNames()

	beforeSet := make(map[string]bool, len(before))
	for _, n := range before {
		beforeSet[n] = true
	}
	var loadedThisTurn []string
	for _, n := range after {
		if !beforeSet[n] {
			loadedThisTurn = append(loadedThisTurn, n)
		}
	}

	slog.Info(
		"Skill turn summary",
		"component", "skills",
		"session_id", sessionID,
		"prompt_len", len(prompt),
		"active_total", len(activeSkills),
		"loaded_total", len(after),
		"loaded_this_turn", loadedThisTurn,
	)
}

// logDiscoveryStats emits a single structured log line summarising skill
// discovery for the current session. It is intentionally low-volume: one
// line per session start. Builtin vs user counts are derived from the
// SkillState.Path — builtin states use the "builtin/" embed prefix.
func logDiscoveryStats(
	states []*skills.SkillState,
	userPaths []string,
	allSkills, activeSkills []*skills.Skill,
	disabled []string,
) {
	var builtinOK, builtinErr, userOK, userErr int
	for _, s := range states {
		isBuiltin := strings.HasPrefix(s.Path, "builtin/")
		switch {
		case isBuiltin && s.State == skills.StateNormal:
			builtinOK++
		case isBuiltin && s.State == skills.StateError:
			builtinErr++
		case !isBuiltin && s.State == skills.StateNormal:
			userOK++
		case !isBuiltin && s.State == skills.StateError:
			userErr++
		}
	}

	activeNames := make([]string, 0, len(activeSkills))
	for _, s := range activeSkills {
		activeNames = append(activeNames, s.Name)
	}

	xml := skills.ToPromptXML(activeSkills)

	slog.Info(
		"Skill discovery complete",
		"component", "skills",
		"builtin_ok", builtinOK,
		"builtin_errors", builtinErr,
		"user_ok", userOK,
		"user_errors", userErr,
		"user_paths", len(userPaths),
		"deduped_total", len(allSkills),
		"active", len(activeSkills),
		"disabled", len(disabled),
		"prompt_bytes", len(xml),
		"prompt_tok_est", skills.ApproxTokenCount(xml),
		"active_names", activeNames,
	)
}
