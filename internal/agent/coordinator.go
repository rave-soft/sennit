package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
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
	// Not used yet; for when there are multiple agents.
	// SetMainAgent(string)
	Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error)
	// RunAccepted runs a call that was already accepted via
	// BeginAccepted on the fire-and-forget dispatch path. The handle is
	// the only carrier of accept-state across the AgentDispatcher.run /
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
	// SetDelegationTools atomically publishes the thread and task tool
	// adapters derived by threadspawn.Attach. Readers always receive one
	// complete adapter generation. A changed thread adapter invalidates the
	// runtime because it changes the thread_* tool set; task adapters take
	// effect immediately for background delegation.
	SetDelegationTools(threads tools.ThreadManager, tasks tools.TaskManager)
	// DeliverTaskCompletion enqueues completion into sessionID's
	// completion inbox for delivery on that session's next step (see
	// runTurn.prepareStep), or starts a continuation turn immediately if
	// the session is idle and eligible (see startContinuation).
	// internal/thread calls this once a task reaches a terminal status,
	// having resolved sessionID as the task's *parent* session - never
	// the task's own child session.
	DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion)
	// RegisterDelegationParent records where sessionID (a running
	// delegation's own child session) should deliver a mid-run ask via
	// SendToParent. internal/thread calls this once, at delegation-
	// create time (a later change - not part of this step).
	RegisterDelegationParent(sessionID string, parent DelegationParent)
	// SendToParent delivers a mid-run ask from sessionID to its
	// registered parent. See SessionAgent.SendToParent.
	SendToParent(ctx context.Context, sessionID, message string) error
	// RefreshSkills replaces the coordinator's cached skill discovery
	// results — called by app.startExternalChangeWatchers (see
	// internal/app/watch.go) after its skills-directory watcher detects a
	// SKILL.md added, edited, or removed outside this process,
	// so a hot-reload takes effect on the next Run without a restart. It
	// preserves the skill tracker's loaded-state for names that are still
	// active rather than resetting it, so a skill already read earlier in
	// the session does not appear to forget itself.
	RefreshSkills(allSkills, activeSkills []*skills.Skill)
}

type delegationToolsSnapshot struct {
	threads         tools.ThreadManager
	tasks           tools.TaskManager
	threadsIdentity managerIdentity
}

type coordinator struct {
	cfg         *config.ConfigStore
	credentials *credentials.Manager
	sessions    session.Service
	messages    MessageService
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
	latency     latency.Recorder

	localVersion atomic.Uint64
	runtime      *runtimeCache

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

	// runtimeInvalidationMu serializes runtime-affecting state mutation and
	// publication of each local generation with its exact invalidation reason.
	// Runtime builds never hold it.
	runtimeInvalidationMu sync.Mutex

	currentAgent SessionAgent
	agents       map[string]SessionAgent

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
	Config *config.ConfigStore
	// Credentials is this process's single OAuth credentials manager
	// (see credentials.Manager's doc comment on why there must be
	// exactly one). Required: WaitForTokenChange and RefreshOAuthToken
	// are called on it during interactive re-authentication and 401
	// retry handling.
	Credentials *credentials.Manager
	Sessions    session.Service
	Messages    MessageService
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
	// states, or auth handlers keyed by MCP server name.
	MCP *mcp.Registry
	// Threads is nil-safe: when nil, the thread_* tools are simply
	// omitted from the top-level agent's tool list. It is normally wired
	// after construction via [Coordinator.SetDelegationTools] instead of here,
	// since the thread manager is set up post-bootstrap; this field
	// exists mainly so tests and other in-process callers can supply one
	// up front.
	Threads tools.ThreadManager
	// Tasks is nil-safe the same way Threads is: when nil, the built-in
	// agent tool's background branch reports background delegation as
	// unavailable rather than silently running in the foreground. Wired
	// after construction via [Coordinator.SetDelegationTools] in production, same
	// as Threads.
	Tasks            tools.TaskManager
	BackgroundShells *shell.BackgroundShellManager
	// Latency is nil-safe: when nil, the handoff waits every session
	// agent observes are logged but not recorded. See internal/latency.
	Latency latency.Recorder
}

func NewCoordinator(ctx context.Context, opts CoordinatorOptions) (Coordinator, error) {
	if opts.BackgroundShells == nil {
		return nil, errBackgroundShellsRequired
	}

	// Skills are pre-discovered by the caller (see app.Bootstrap) and
	// passed in via the manager. If no
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
		cfg:             opts.Config,
		credentials:     opts.Credentials,
		sessions:        opts.Sessions,
		messages:        opts.Messages,
		permissions:     opts.Permissions,
		questions:       opts.Questions,
		history:         opts.History,
		filetracker:     opts.FileTracker,
		lspManager:      opts.LSPManager,
		notify:          opts.Notify,
		runComplete:     opts.RunComplete,
		agents:          make(map[string]SessionAgent),
		allSkills:       allSkills,
		activeSkills:    activeSkills,
		skillTracker:    skillTracker,
		skillsMgr:       opts.Skills,
		interactive:     opts.Interactive,
		mcp:             opts.MCP,
		delegationTools: atomic.Pointer[delegationToolsSnapshot]{},
		background:      opts.BackgroundShells,
		latency:         opts.Latency,
		runtime:         newRuntimeCache(),
	}
	c.delegationTools.Store(&delegationToolsSnapshot{
		threads: opts.Threads, tasks: opts.Tasks, threadsIdentity: managerIdentityOf(opts.Threads),
	})

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
	// An auto-woken continuation is a real turn and needs a real turn's
	// runtime — see runContinuation. Wired here rather than inside
	// buildAgent because only the coordinator's own agent ever runs one.
	if sa, ok := agent.(*sessionAgent); ok {
		sa.continuationRunner = c.runContinuation
	}
	return c, nil
}

// runContinuation dispatches an auto-woken continuation turn for
// sessionID through the same preparation a prompted turn gets: MCP
// initialization waited out, a runtime resolved from the current config,
// an expired OAuth token refreshed first, and the model's own call
// options carried onto the call.
//
// It exists because the wake path used to go straight to
// sessionAgent.Run with nothing but the session id and a placeholder
// prompt: no Runtime, so no thinking options and no output-token budget;
// no OnAuthRefresh, so an OAuth token that expired while a delegation ran
// produced a 401 with no retry; and no MCP wait, so a continuation that
// woke early could be built without the tools it needed. Every one of
// those is most likely precisely when a continuation fires — long after
// the turn that started the delegation.
func (c *coordinator) runContinuation(ctx context.Context, sessionID string) error {
	if err := c.readyWg.Wait(); err != nil {
		return err
	}
	if err := c.waitForMCPInit(ctx); err != nil {
		return fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	runtime, err := c.runtimeFor(ctx)
	if err != nil {
		return fmt.Errorf("failed to prepare agent runtime: %w", err)
	}
	if err := c.refreshTokenIfExpired(ctx, runtime.providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token for a continuation. Proceeding with existing token.", "error", err)
	} else if c.runtimeKey() != runtime.key {
		runtime, err = c.runtimeFor(ctx)
		if err != nil {
			return fmt.Errorf("failed to prepare refreshed agent runtime: %w", err)
		}
	}
	active := newActiveRuntime(runtime)

	_, err = c.currentAgent.Run(ctx, SessionAgentCall{
		Runtime:          runtime,
		ActiveRuntime:    active,
		SessionID:        sessionID,
		Prompt:           continuationPromptPlaceholder,
		Continuation:     true,
		MaxOutputTokens:  runtime.maxOutputTokens,
		ProviderOptions:  withPromptCacheKey(runtime.providerOptions, runtime.model, runtime.providerCfg, sessionID),
		Temperature:      runtime.temperature,
		TopP:             runtime.topP,
		TopK:             runtime.topK,
		FrequencyPenalty: runtime.frequencyPenalty,
		PresencePenalty:  runtime.presencePenalty,
		OnAuthRefresh:    c.makeAuthRefreshCallback(runtime.providerCfg, active),
	})
	return err
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
	// though sennit_info reports them as connected.
	if err := c.waitForMCPInit(ctx); err != nil {
		return nil, fmt.Errorf("failed to wait for MCP initialization: %w", err)
	}

	runtime, err := c.runtimeFor(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare agent runtime: %w", err)
	}

	if err := c.refreshTokenIfExpired(ctx, runtime.providerCfg); err != nil {
		// We don't return here because the event handling to ask the user to reauthenticate
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
	// retry's success, and `sennit run` would exit on the stale error
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
	// at the dispatch boundary in AgentDispatcher.Send) onto the
	// SessionAgentCall so the terminal RunComplete event echoes it
	// back. Both attempts in the retry chain reuse the same RunID;
	// the coalesce closure publishes the final outcome under that
	// same correlator.
	runID := RunIDFromContext(ctx)
	promptOrigin := PromptOriginFromContext(ctx)
	// A steering dispatch (agent.WithSteering) asks for this prompt to
	// reach a turn already in flight rather than queue behind it. The
	// decision hook is wrapped in a Once because the auth-retry chain
	// below may call run twice: the retry's dispatch decision is not a
	// second event to report - the first attempt already reached one, and
	// an auth failure happens while streaming, long past it.
	onDispatch, steering := SteeringFromContext(ctx)
	if onDispatch != nil {
		var once sync.Once
		hook := onDispatch
		onDispatch = func(outcome SteerOutcome) { once.Do(func() { hook(outcome) }) }
	}
	run := func() (*fantasy.AgentResult, error) {
		return c.currentAgent.Run(ctx, SessionAgentCall{
			Runtime:          runtime,
			ActiveRuntime:    active,
			SessionID:        sessionID,
			RunID:            runID,
			Prompt:           prompt,
			PromptOrigin:     promptOrigin,
			Steering:         steering,
			OnDispatch:       onDispatch,
			Attachments:      attachments,
			MaxOutputTokens:  runtime.maxOutputTokens,
			ProviderOptions:  withPromptCacheKey(runtime.providerOptions, runtime.model, runtime.providerCfg, sessionID),
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
		// Detached, with a bounded deadline of its own: this is the
		// authoritative terminal event, and the commonest reason to be
		// publishing it is that the run was cancelled — which cancels
		// ctx too, so publishing on it dropped the very event a caller
		// waiting on this RunID needs. The deferred publisher inside
		// sessionAgent.run already detaches for the same reason.
		publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(ctx), runCompletePublishTimeout)
		c.runComplete.PublishMustDeliver(publishCtx, pubsub.UpdatedEvent, latest)
		cancelPublish()
		// Signal to the dispatcher (AgentDispatcher.run) that the
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
// runCompletePublishTimeout bounds the detached publish of a coalesced
// terminal RunComplete. PublishMustDeliver waits for a slow subscriber
// rather than dropping the event, so it needs a deadline that is not the
// run's own.
const runCompletePublishTimeout = 5 * time.Second

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
		AutoSummarizeAt:      c.cfg.Config().Options.AutoSummarizeAt,
		Sessions:             c.sessions,
		Messages:             c.messages,
		Tools:                nil,
		Notify:               c.notify,
		RunComplete:          c.runComplete,
		MCP:                  c.mcp,
		Latency:              c.latency,
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

// buildTools assembles the tool set an agent build gets, from toolSpecs
// (tool_registry.go) plus two groups that can't be fixed rows there: the
// user-defined agent tools (one per config.Agents entry — never offered
// to a sub-agent, or delegation could recurse without bound) and the
// per-MCP-server tools (gated by AllowedMCP, not AllowedTools).
//
// c.cfg.Config() is read exactly once, into an agentConfig snapshot (see
// newAgentConfig): ConfigStore.Config() takes no lock spanning multiple
// calls, so reading it repeatedly across one build let a concurrent
// config reload hand different tools in the same set different values.
// One snapshot means every tool here sees the same config.
func (c *coordinator) buildTools(ctx context.Context, agent config.Agent, isSubAgent bool) ([]fantasy.AgentTool, error) {
	cfg := newAgentConfig(c.cfg.Config())

	searchBackend, err := c.webSearchBackend()
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}

	allSkillsSnapshot, activeSkillsSnapshot, skillTrackerSnapshot := c.skillsSnapshot()

	delegationTools := c.delegationToolsForRead()
	b := &buildToolsCtx{
		agent:              agent,
		isSubAgent:         isSubAgent,
		interactive:        c.interactive,
		cfg:                cfg,
		modelID:            cfg.ModelID(),
		logFile:            config.GlobalLogFile(),
		searchBackend:      searchBackend,
		allSkills:          allSkillsSnapshot,
		activeSkills:       activeSkillsSnapshot,
		skillTracker:       skillTrackerSnapshot,
		threads:            delegationTools.threads,
		taskManager:        delegationTools.tasks,
		backgroundAgentsOn: c.backgroundAgentsEnabled(),
		toolAvailability:   tools.ResolveSystemToolAvailability(),
	}

	var allTools []fantasy.AgentTool
	for _, spec := range toolSpecs() {
		gate, ok := specGate(spec)
		if !ok || !gateAllows(gate, spec.Names[0], b) {
			continue
		}
		built, err := spec.Build(ctx, c, b)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, built...)
	}

	// User-defined agents are offered to the top-level agent only (see
	// this function's doc comment).
	if !isSubAgent {
		customTools, err := c.customAgentTools(ctx, cfg)
		if err != nil {
			return nil, err
		}
		allTools = append(allTools, customTools...)
	}

	// Build hook runner if PreToolUse hooks are configured.
	var hookRunner *hooks.Runner
	if preToolHooks := cfg.PreToolUseHooks(); len(preToolHooks) > 0 {
		hookRunner = hooks.NewRunner(preToolHooks, c.cfg.WorkingDir(), c.cfg.WorkingDir())
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

// SetDelegationTools implements Coordinator. One immutable snapshot makes
// mixed adapter generations impossible, including while publications race.
func (c *coordinator) SetDelegationTools(threads tools.ThreadManager, tasks tools.TaskManager) {
	identity := managerIdentityOf(threads)
	c.invalidateRuntime(context.Background(), "threads_changed", func() bool {
		current := c.delegationTools.Load()
		c.delegationTools.Store(&delegationToolsSnapshot{
			threads: threads, tasks: tasks, threadsIdentity: identity,
		})
		return current == nil || !current.threadsIdentity.same(identity)
	})
}

// DeliverTaskCompletion implements Coordinator.
func (c *coordinator) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	c.currentAgent.DeliverTaskCompletion(ctx, sessionID, completion)
}

// RegisterDelegationParent implements Coordinator.
func (c *coordinator) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	c.currentAgent.RegisterDelegationParent(sessionID, parent)
}

// SendToParent implements Coordinator.
func (c *coordinator) SendToParent(ctx context.Context, sessionID, message string) error {
	return c.currentAgent.SendToParent(ctx, sessionID, message)
}

type managerIdentity struct {
	typ           reflect.Type
	comparable    any
	ptr           uintptr
	alwaysChanged bool
}

func (i managerIdentity) same(other managerIdentity) bool {
	if i.alwaysChanged || other.alwaysChanged || i.typ != other.typ {
		return false
	}
	if i.comparable != nil || other.comparable != nil {
		return i.comparable == other.comparable
	}
	return i.ptr == other.ptr
}

// managerIdentityOf provides stable identity for implementations backed by
// comparable values and map, slice, function, pointer, or chan values. The
// latter cannot be compared through an interface without panicking.
// managerIdentityOf avoids interface equality for non-comparable managers.
// Maps expose stable identity. Functions, slices, and unknown non-comparable
// values deliberately force a rebuild: a function pointer identifies code, not
// captured closure state, and the others have no stable interface identity.
func managerIdentityOf(manager tools.ThreadManager) managerIdentity {
	if manager == nil {
		return managerIdentity{}
	}
	value := reflect.ValueOf(manager)
	identity := managerIdentity{typ: value.Type()}
	if value.Type().Comparable() {
		identity.comparable = manager
		return identity
	}
	switch value.Kind() {
	case reflect.Map:
		identity.ptr = value.Pointer()
	case reflect.Func:
		identity.alwaysChanged = true
	default:
		identity.alwaysChanged = true
	}
	return identity
}

type effectiveSkill struct {
	Name, Description, SkillFilePath string
	DisableModelInvocation, Builtin  bool
}

// sameSkills includes only fields emitted into the runtime skill prompt.
// Discovery details and skill content are consumed by the tracker at skill-use
// time, rather than by compiled tool/prompt construction.
func sameSkills(a, b []*skills.Skill) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] == nil || b[i] == nil {
			if a[i] != b[i] {
				return false
			}
			continue
		}
		left := effectiveSkill{a[i].Name, a[i].Description, a[i].SkillFilePath, a[i].DisableModelInvocation, a[i].Builtin}
		right := effectiveSkill{b[i].Name, b[i].Description, b[i].SkillFilePath, b[i].DisableModelInvocation, b[i].Builtin}
		if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

// RefreshSkills implements Coordinator.RefreshSkills.
func (c *coordinator) RefreshSkills(allSkills, activeSkills []*skills.Skill) {
	c.invalidateRuntime(context.Background(), "skills_changed", func() bool {
		c.skillsMu.Lock()
		changed := !sameSkills(c.allSkills, allSkills) || !sameSkills(c.activeSkills, activeSkills)
		c.allSkills = allSkills
		c.activeSkills = activeSkills
		// The tracker itself is not replaced: UpdateActiveSkills mutates it
		// in place under its own lock, keeping loaded state for names still
		// active rather than wiping it (see UpdateActiveSkills).
		tracker := c.skillTracker
		c.skillsMu.Unlock()
		tracker.UpdateActiveSkills(activeSkills)
		return changed
	})
}

// skillStates returns the workspace's current skill discovery states, or
// nil when no skills manager was wired in. Used for sennit_info's
// [problems] section, so an agent can see that a SKILL.md it was told to
// follow never loaded — see config.SkillProblems for why that particular
// failure is worth surfacing to the agent and not only to the log.
func (c *coordinator) skillStates() []*skills.SkillState {
	if c.skillsMgr == nil {
		return nil
	}
	return c.skillsMgr.States()
}

// skillsSnapshot returns the coordinator's current skill discovery
// results under skillsMu, for callers (buildTools, Run) that need a
// consistent read while RefreshSkills may be running concurrently.
func (c *coordinator) skillsSnapshot() (allSkills, activeSkills []*skills.Skill, tracker *skills.Tracker) {
	c.skillsMu.RLock()
	defer c.skillsMu.RUnlock()
	return c.allSkills, c.activeSkills, c.skillTracker
}

// delegationToolsForRead returns a complete adapter generation.
func (c *coordinator) delegationToolsForRead() delegationToolsSnapshot {
	if snapshot := c.delegationTools.Load(); snapshot != nil {
		return *snapshot
	}
	return delegationToolsSnapshot{}
}

// threadsManager returns the thread adapter from one delegation generation.
func (c *coordinator) threadsManager() tools.ThreadManager {
	return c.delegationToolsForRead().threads
}

// tasksManager returns the task adapter from one delegation generation.
func (c *coordinator) tasksManager() tools.TaskManager {
	return c.delegationToolsForRead().tasks
}

// backgroundAgentsEnabled reports whether options.background_agents allows
// *new* background dispatch right now. It is read fresh on every call —
// never cached — so a config reload takes effect for the next dispatch
// without touching a task that is already running: that task's own runtime
// state lives in the task manager, not here, and this gate is never
// consulted again once a task has started.
func (c *coordinator) backgroundAgentsEnabled() bool {
	enabled := c.cfg.Config().Options.BackgroundAgents
	return enabled == nil || *enabled
}

func (c *coordinator) IsBusy() bool {
	return c.currentAgent.IsBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	if c.currentAgent.IsSessionBusy(sessionID) {
		return true
	}
	c.subSessionsMu.Lock()
	defer c.subSessionsMu.Unlock()
	return c.subSessions[sessionID] > 0
}

// markSubSessionBusy records that a delegation is running under
// sessionID and returns the release to call when it finishes. The
// release is idempotent, and the counter (rather than a set) keeps a
// re-run of the same delegation — a retried tool call reuses the
// session id, which is derived from the message and tool-call ids —
// from clearing the flag out from under the run still in flight.
func (c *coordinator) markSubSessionBusy(sessionID string) func() {
	c.subSessionsMu.Lock()
	if c.subSessions == nil {
		c.subSessions = make(map[string]int)
	}
	c.subSessions[sessionID]++
	c.subSessionsMu.Unlock()

	var released bool
	return func() {
		if released {
			return
		}
		released = true
		c.subSessionsMu.Lock()
		defer c.subSessionsMu.Unlock()
		if c.subSessions[sessionID] <= 1 {
			delete(c.subSessions, sessionID)
			return
		}
		c.subSessions[sessionID]--
	}
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

func (c *coordinator) invalidateRuntime(ctx context.Context, reason string, mutate func() bool) {
	c.runtimeInvalidationMu.Lock()
	defer c.runtimeInvalidationMu.Unlock()
	if !mutate() {
		return
	}
	nextVersion := c.localVersion.Load() + 1
	nextKey := c.runtimeKey()
	nextKey.local = nextVersion
	if c.runtime != nil {
		c.runtime.invalidateAndPublish(ctx, reason, nextKey, func() {
			c.localVersion.Store(nextVersion)
		})
		return
	}
	c.localVersion.Store(nextVersion)
}

func (c *coordinator) runtimeFor(ctx context.Context) (*compiledRuntime, error) {
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
		maxTokens := modelMaxOutputTokens(model)
		return &compiledRuntime{
			key: key, model: model, tools: builtTools, systemPrompt: systemPrompt,
			providerCfg: providerCfg, providerOptions: options,
			temperature: temp, topP: topP, topK: topK,
			frequencyPenalty: freqPenalty, presencePenalty: presPenalty,
			maxOutputTokens:      maxTokens,
			systemPromptPrefix:   providerCfg.SystemPromptPrefix,
			disableAutoSummarize: c.cfg.Config().Options.DisableAutoSummarize,
			autoSummarizeAt:      c.cfg.Config().Options.AutoSummarizeAt,
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

	// The summary request replays the same conversation prefix the turns
	// did, so it wants the same routing (see withPromptCacheKey) — and it
	// is the single most expensive request a session makes, since a
	// summary is only asked for once the context is full.
	summaryOptions := withPromptCacheKey(runtime.providerOptions, runtime.model, runtime.providerCfg, sessionID)
	if agent, ok := c.currentAgent.(*sessionAgent); ok {
		return agent.summarize(ctx, sessionID, summaryOptions, c.makeAuthRefreshCallback(runtime.providerCfg, active), runtime.model, runtime.systemPromptPrefix, active, nil)
	}
	return c.currentAgent.Summarize(ctx, sessionID, summaryOptions, c.makeAuthRefreshCallback(runtime.providerCfg, active))
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
// production call sites (app.Bootstrap) run discovery in advance and
// pass the results via the manager;
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
