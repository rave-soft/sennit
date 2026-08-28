package agent

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/agent/tools/mcp"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/filetracker"
	historystore "github.com/rave-soft/sennit/internal/history/store"
	"github.com/rave-soft/sennit/internal/latency"
	"github.com/rave-soft/sennit/internal/lsp"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/skills"
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
	// runTurn.prepareStep). A delegation's own session, being work
	// nobody is sitting in, is also woken for it if it is idle (see
	// startContinuation and isDelegationSession); a session a person
	// drives is never started by this - the completion waits for their
	// next turn.
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

// coordinator is the public facade of the workspace agent: it composes the
// three components that own the real work and forwards the Coordinator API
// to its owner.
//
//   - runtimeBuilder (builder): per-turn runtime construction and
//     resolution — the compiled-runtime cache, model/provider/tool/prompt
//     assembly, credential refresh.
//   - turnDispatcher (dispatcher): the top-level agent's turn lifecycle —
//     Run/Steer/queue/cancel, agent construction and readiness, Close.
//   - delegationFinalizer (delegation): delegation launch/run/finalization
//     and the skill + thread/task adapter state the tool builds read.
//
// The facade owns no turn, runtime, or delegation state of its own: every
// piece of mutable state lives in exactly one component. The service and
// config handles below are shared read-only dependencies, handed to the
// components at construction rather than referenced through the facade.
type coordinatorAgentPort struct{ agent SessionAgent }

func (p *coordinatorAgentPort) current() SessionAgent  { return p.agent }
func (p *coordinatorAgentPort) set(agent SessionAgent) { p.agent = agent }

type coordinator struct {
	cfg         *config.ConfigStore
	credentials *credentials.Manager
	sessions    sessionstore.Service
	messages    MessageService
	permissions permission.Requester
	questions   question.Service
	history     historystore.Service
	filetracker filetracker.Service
	lspManager  *lsp.Manager
	notify      pubsub.Publisher[notify.Notification]
	runComplete pubsub.Publisher[notify.RunComplete]
	interactive bool
	mcp         *mcp.Registry
	background  *shell.BackgroundShellManager
	latency     latency.Recorder

	builder    *runtimeBuilder
	dispatcher *turnDispatcher
	delegation *delegationFinalizer
}

// newCoordinatorComponents constructs and wires the coordinator's three
// components from the facade's own dependencies. It must be called before
// any component is used; production construction happens in NewCoordinator,
// and tests that build a coordinator as a bare struct literal call it
// directly.
//
// The dispatcher is constructed first, before any method value of it is
// captured: a Go method value freezes its receiver at capture time, so
// taking delegation.buildAgent = dispatcher.buildAgent while dispatcher is
// still nil would permanently hand the delegation path a nil receiver and
// panic at the first task/custom-agent launch.
func (c *coordinator) newCoordinatorComponents() {
	agentPort := &coordinatorAgentPort{}
	lifecycle := &readinessLifecycle{}
	c.dispatcher = &turnDispatcher{
		cfg:          c.cfg,
		lastActivity: csync.NewMap[string, time.Time](),
		sessions:     c.sessions,
		messages:     c.messages,
		notify:       c.notify,
		runComplete:  c.runComplete,
		mcp:          c.mcp,
		latency:      c.latency,
		agentPort:    agentPort,
		lifecycle:    lifecycle,
	}

	c.builder = &runtimeBuilder{
		cfg:         c.cfg,
		credentials: c.credentials,
		notify:      c.notify,
		mcp:         c.mcp,
		interactive: c.interactive,
		runtime:     newRuntimeCache(),
	}

	c.delegation = &delegationFinalizer{
		cfg:         c.cfg,
		sessions:    c.sessions,
		messages:    c.messages,
		permissions: c.permissions,
		questions:   c.questions,
		history:     c.history,
		filetracker: c.filetracker,
		lspManager:  c.lspManager,
		background:  c.background,
		notify:      c.notify,
		runComplete: c.runComplete,
		mcp:         c.mcp,
		latency:     c.latency,
		agentPort:   agentPort,
		lifecycle:   lifecycle,
	}

	// Wire every cross-component seam now that all three exist. The
	// method values below capture their receivers at this moment, so the
	// construction order above is load-bearing (see the function's doc
	// comment).
	c.dispatcher.builder = c.builder
	c.dispatcher.delegation = c.delegation
	c.delegation.builder = c.builder
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
	Sessions    sessionstore.Service
	Messages    MessageService
	Permissions permission.Requester
	Questions   question.Service
	History     historystore.Service
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
		cfg:         opts.Config,
		credentials: opts.Credentials,
		sessions:    opts.Sessions,
		messages:    opts.Messages,
		permissions: opts.Permissions,
		questions:   opts.Questions,
		history:     opts.History,
		filetracker: opts.FileTracker,
		lspManager:  opts.LSPManager,
		notify:      opts.Notify,
		runComplete: opts.RunComplete,
		interactive: opts.Interactive,
		mcp:         opts.MCP,
		background:  opts.BackgroundShells,
		latency:     opts.Latency,
	}
	c.newCoordinatorComponents()

	c.delegation.allSkills = allSkills
	c.delegation.activeSkills = activeSkills
	c.delegation.skillTracker = skillTracker
	c.delegation.skillsMgr = opts.Skills

	c.delegation.SetDelegationTools(opts.Threads, opts.Tasks)

	agentCfg, ok := opts.Config.Config().Agents[config.AgentCoder]
	if !ok {
		return nil, errCoderAgentNotConfigured
	}

	// TODO: make this dynamic when we support multiple agents
	prompt, err := coderPrompt(prompt.WithWorkingDir(c.cfg.WorkingDir()))
	if err != nil {
		return nil, err
	}

	agent, err := c.delegation.buildAgent(ctx, prompt, agentCfg, false)
	if err != nil {
		return nil, err
	}
	c.dispatcher.agentPort.set(agent)
	// An auto-woken continuation is a real turn and needs a real turn's
	// runtime — see runContinuation. Wired here rather than inside
	// buildAgent because only the coordinator's own agent ever runs one.
	if sa, ok := agent.(*sessionAgent); ok {
		sa.continuationRunner = c.dispatcher.runContinuation
	}
	// Started last, once there is an agent for it to summarize through.
	// It ends with the coordinator: Close cancels the lifecycle context
	// the sweep selects on and waits for it. See startIdleSummarize.
	c.dispatcher.startIdleSummarize()
	return c, nil
}

// RunComplete publish timeout. PublishMustDeliver waits for a slow
// subscriber rather than dropping the event, so the detached publish of a
// coalesced terminal RunComplete (see turnDispatcher.run) needs a deadline
// that is not the run's own.
const runCompletePublishTimeout = 5 * time.Second

// --- delegation adapter identity and skill comparison -----------------------

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

// --- public facade: turn lifecycle --------------------------------------

// Run implements Coordinator.
func (c *coordinator) Run(ctx context.Context, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.dispatcher.run(ctx, nil, sessionID, prompt, attachments...)
}

// RunAccepted implements Coordinator.
func (c *coordinator) RunAccepted(ctx context.Context, accept *AcceptedRun, sessionID string, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	return c.dispatcher.run(ctx, accept, sessionID, prompt, attachments...)
}

// Steer implements Coordinator.
func (c *coordinator) Steer(ctx context.Context, call SessionAgentCall) (SteerOutcome, *fantasy.AgentResult, error) {
	return c.dispatcher.Steer(ctx, call)
}

// BeginAccepted reserves an accept slot for sessionID on the active
// agent and returns the ownership handle. It is the fire-and-forget
// dispatch path's only way to mark a run as accepted-but-not-yet-active
// so a cancel arriving before the run registers in activeRequests is not
// lost.
func (c *coordinator) BeginAccepted(sessionID string) *AcceptedRun {
	return c.dispatcher.BeginAccepted(sessionID)
}

func (c *coordinator) Cancel(sessionID string) {
	c.dispatcher.Cancel(sessionID)
}

func (c *coordinator) CancelAll() {
	c.dispatcher.CancelAll()
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.dispatcher.ClearQueue(sessionID)
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
	return c.dispatcher.Close(ctx)
}

// --- public facade: model and title --------------------------------------

func (c *coordinator) Model() Model {
	return c.dispatcher.Model()
}

// UpdateModels re-resolves the main agent's model from the current config
// and hands it, with its tools and system prompt, to the current agent.
func (c *coordinator) UpdateModels(ctx context.Context) error {
	if c.dispatcher.agentPort.current() == nil {
		return nil
	}
	return c.dispatcher.runUpdateModels(ctx, c.dispatcher.agentPort.current())
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.dispatcher.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.dispatcher.QueuedPromptsList(sessionID)
}

// Summarize resolves the runtime the summary request replays through,
// refreshes an expired OAuth token first, and hands the call to the
// current agent's own summarize pass (see turnDispatcher.Summarize).
func (c *coordinator) Summarize(ctx context.Context, sessionID string) error {
	return c.dispatcher.Summarize(ctx, sessionID)
}

// GenerateTitle generates a session title using the current agent, with
// the model and prefix the runtime was compiled from.
func (c *coordinator) GenerateTitle(ctx context.Context, sessionID, prompt string) {
	c.dispatcher.GenerateTitle(ctx, sessionID, prompt)
}

// --- public facade: delegation and skills --------------------------------

// SetDelegationTools implements Coordinator. One immutable snapshot makes
// mixed adapter generations impossible, including while publications race.
func (c *coordinator) SetDelegationTools(threads tools.ThreadManager, tasks tools.TaskManager) {
	c.delegation.SetDelegationTools(threads, tasks)
}

// DeliverTaskCompletion implements Coordinator.
func (c *coordinator) DeliverTaskCompletion(ctx context.Context, sessionID string, completion TaskCompletion) {
	c.dispatcher.agentPort.current().DeliverTaskCompletion(ctx, sessionID, completion)
}

// RegisterDelegationParent implements Coordinator.
func (c *coordinator) RegisterDelegationParent(sessionID string, parent DelegationParent) {
	c.dispatcher.agentPort.current().RegisterDelegationParent(sessionID, parent)
}

// SendToParent implements Coordinator.
func (c *coordinator) SendToParent(ctx context.Context, sessionID, message string) error {
	return c.dispatcher.agentPort.current().SendToParent(ctx, sessionID, message)
}

// IsBusy reports whether the coordinator's own top-level agent is running.
func (c *coordinator) IsBusy() bool {
	return c.dispatcher.agentPort.current().IsBusy()
}

// IsSessionBusy reports whether sessionID is busy: either the top-level
// agent's own dispatch state says so, or a delegation is running under
// that session (tracked by the delegation finalizer's sub-session
// counter).
func (c *coordinator) IsSessionBusy(sessionID string) bool {
	if c.dispatcher.agentPort.current().IsSessionBusy(sessionID) {
		return true
	}
	c.delegation.subSessionsMu.Lock()
	defer c.delegation.subSessionsMu.Unlock()
	return c.delegation.subSessions[sessionID] > 0
}

// RefreshSkills replaces the cached skill discovery results and
// invalidates the runtime when the effective skill set changed (see
// delegationFinalizer.RefreshSkills).
func (c *coordinator) RefreshSkills(allSkills, activeSkills []*skills.Skill) {
	c.delegation.RefreshSkills(allSkills, activeSkills)
}

// --- skill discovery fallback ---------------------------------------------

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
