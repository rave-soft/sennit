package mcp

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// Registry owns all state for a set of MCP client connections: sessions,
// per-server status, the event broker, OAuth handlers in flight, and the
// aggregated tool/prompt/resource catalogs served to the agent.
//
// Historically this state lived in package-level variables, which meant a
// single process could only ever run one MCP registry: a second workspace's
// app.New silently stomped the first's sessions, and closing either
// workspace shut down the broker for both. Every app.App now constructs
// and owns its own *Registry (app.App.MCP), so two workspaces in one
// process no longer share sessions, states, auth handlers, or the event
// broker. The package-level functions below still operate against a
// single [defaultRegistry] purely for source compatibility with tests and
// any caller that constructs a Registry directly rather than via app.New;
// production call sites all go through app.App.MCP.
type attemptID struct {
	gen uint64
	seq uint64
}

// ConfigProvider is the slice of *config.ConfigStore this package needs: the
// dictionary reads (Config, Resolver, Overrides) and the MCP OAuth token
// mutation calls. Declaring it here rather than accepting the concrete
// *config.ConfigStore keeps this package's dependency on config narrow (ISP).
type ConfigProvider interface {
	Config() *config.Config
	Resolver() config.VariableResolver
	Overrides() config.RuntimeOverrides
	ReserveMCPTokenMutation(name string, expected config.MCPConfig) (config.MCPTokenMutation, bool)
	SetMCPToken(reservation *config.MCPTokenMutation, token *oauth.Token) (bool, error)
	ClearMCPToken(reservation *config.MCPTokenMutation, expectedToken *oauth.Token) (bool, error)
}

var _ ConfigProvider = (*config.ConfigStore)(nil)

func (a attemptID) valid() bool { return a.seq != 0 }

// serverLocks groups the two per-server mutexes a Registry needs: one to
// serialize lazy session renewal, one to serialize browser-suppressed OAuth.
// A server's pair lives behind a single map lookup instead of two, so the
// locking a server needs is a fixed set of fields on one struct rather than
// two independently grown-and-shrunk maps that happen to be keyed the same
// way by convention.
type serverLocks struct {
	renew    sync.Mutex
	suppress sync.Mutex
}

// serverLock returns the lock pair for name, creating it on first use.
func (r *Registry) serverLock(name string) *serverLocks {
	return r.locks.GetOrSet(name, func() *serverLocks { return &serverLocks{} })
}

type tokenWriteOwner struct {
	name    string
	attempt attemptID
}

type tokenWrite struct {
	done chan struct{}
}

// Registry is split three ways, but publishMu — the lock that makes an
// attempt's "do I still own this server" check indivisible from the
// catalog/session/state write that follows it — stays a single field
// on Registry itself, never duplicated or moved. connectionManager and
// authCoordinator are separate types for per-server session lifecycle and
// the OAuth flow respectively, but neither holds a lock of its own over
// owners/sessionOwners/closing/tokenReservations/tokenWrites: every method
// that needs to check or change ownership reaches back through the reg
// field they each carry and calls a Registry method (owns, ownsLocked,
// ownsSessionLocked, publishOrClose/publishSession, teardown, ...). Because
// Registry embeds both sub-types anonymously, that reference is symmetric —
// a connectionManager method can call a Registry method or an
// authCoordinator method through the same `cm.reg.Foo(...)` spelling — so
// there is exactly one lock and exactly one nesting order for it to
// participate in: publishMu, and where catalogMu is also needed,
// publishMu -> catalogMu, always taken by a Registry-owned method. Neither
// connectionManager nor authCoordinator ever takes catalogMu itself without
// first being inside a publishMu-holding Registry call, so the ordering
// cannot be reversed by construction.
type Registry struct {
	sessions    *csync.Map[string, *ClientSession]
	states      *csync.Map[string, ClientInfo]
	authURLs    *csync.Map[string, authPublication]
	authAttempt atomic.Uint64
	broker      *pubsub.Broker[Event]

	initMu      sync.Mutex
	initOnce    sync.Once
	initDone    chan struct{}
	initStarted bool

	// locks holds two mutexes per server: renew (serializes lazy session
	// renewals so concurrent tool calls cannot race to rebuild the same
	// session) and suppress (serializes browser-suppression so only one
	// remote, server-driven OAuth flow is active for a server at a time).
	// They are grouped on one per-server struct, keyed by one map, rather
	// than kept as two independently-guarded mutex maps: two servers never
	// contend with each other, and the compiler enforces that "the renewal
	// lock" and "the suppress lock" for a given server always mean the same
	// two fields instead of two hand-maintained lookups that could drift.
	locks *csync.Map[string, *serverLocks]

	// gens hands out a per-server generation number. teardown bumps a
	// server's generation; an init goroutine captures it at launch and only
	// commits its session if the generation is still current.
	gens *csync.Map[string, uint64]

	// catalogMu makes the three catalogs and their version a single snapshot.
	catalogMu sync.RWMutex
	version   atomic.Uint64

	// publishMu serializes every externally visible per-server publication.
	// A generation check and the corresponding session, catalog, count and state
	// update must be indivisible: otherwise teardown can observe and clear an old
	// snapshot while its connector publishes it afterwards.
	//
	// This is the one lock every "does this attempt still own the server"
	// check shares with the commit that follows it, so it lives here on
	// Registry rather than on either of the two types below: both
	// connectionManager and authCoordinator hold a *Registry back-reference
	// and go through Registry's owns-check-then-commit methods instead of
	// taking a lock of their own over this state.
	publishMu     sync.Mutex
	owners        map[string]attemptID
	sessionOwners map[string]attemptID
	closing       bool

	tokenWrites        map[tokenWriteOwner]map[*tokenWrite]struct{}
	tokenReservations  map[tokenWriteOwner]*config.MCPTokenMutation
	tokenPersist       func(context.Context, ConfigProvider, string, any) error
	tokenCommit        func(ConfigProvider, *config.MCPTokenMutation, *oauth.Token) error
	beforeTokenPersist func()

	allTools     *csync.Map[string, []*Tool]
	allResources *csync.Map[string, []*Resource]
	allPrompts   *csync.Map[string, []*Prompt]

	// connectionManager owns per-server session lifecycle (connect, renew,
	// reconcile, disable). authCoordinator owns the OAuth/auth flow (BeginAuth,
	// AuthenticateMCP, auth flow bookkeeping). Both are embedded anonymously
	// so their methods and seam fields (newSession, ping, listResources,
	// runAuth, authMu, authFlows, ...) are promoted onto Registry exactly as
	// they were before the split: every existing `r.Foo` call site — in this
	// package's tests and in the rest of the codebase — keeps compiling
	// unchanged. Each sub-type's own methods reach Registry's state through
	// its `reg *Registry` field rather than by embedding Registry back,
	// which is what keeps this a one-directional reference instead of a
	// second, competing lock owner.
	*connectionManager
	*authCoordinator

	// reinitMu guards reinitRunning and reinitDirty.
	reinitMu      sync.Mutex
	reinitRunning bool
	reinitDirty   bool

	// refMu and liveWorkspaces gate broker.Shutdown so closing one
	// workspace's registration does not tear down the shared event stream
	// out from under another workspace still using this same registry.
	//
	// Now that each app.App owns a dedicated *Registry, ArmInit/Close are
	// called exactly once per Registry in production, so this refcount
	// trivially goes 0->1->0 there. It stays in place because defaultRegistry
	// is still a genuinely shared *Registry (see its doc), and any caller
	// that arms it (e.g. a test exercising the package-level API directly)
	// still needs the shutdown-gating this provides.
	refMu          sync.Mutex
	liveWorkspaces int
}

// NewRegistry creates an empty, ready-to-use MCP registry.
func NewRegistry() *Registry {
	r := &Registry{
		sessions:          csync.NewMap[string, *ClientSession](),
		states:            csync.NewMap[string, ClientInfo](),
		authURLs:          csync.NewMap[string, authPublication](),
		broker:            pubsub.NewBroker[Event](),
		initDone:          make(chan struct{}),
		locks:             csync.NewMap[string, *serverLocks](),
		gens:              csync.NewMap[string, uint64](),
		tokenWrites:       map[tokenWriteOwner]map[*tokenWrite]struct{}{},
		tokenReservations: map[tokenWriteOwner]*config.MCPTokenMutation{},
		owners:            map[string]attemptID{},
		sessionOwners:     map[string]attemptID{},
		allTools:          csync.NewMap[string, []*Tool](),
		allResources:      csync.NewMap[string, []*Resource](),
		allPrompts:        csync.NewMap[string, []*Prompt](),
	}
	r.connectionManager = &connectionManager{reg: r}
	r.authCoordinator = &authCoordinator{reg: r, authFlows: map[string]*authFlow{}}
	r.newSession = r.createSession
	r.runAuth = r.runAuthFlow
	r.ping = r.pingSession
	r.listResources = getResources
	r.tokenPersist = func(context.Context, ConfigProvider, string, any) error { return nil }
	r.tokenCommit = func(cfg ConfigProvider, reservation *config.MCPTokenMutation, token *oauth.Token) error {
		_, err := cfg.SetMCPToken(reservation, token)
		return err
	}
	return r
}

// defaultRegistry is the shared, process-wide MCP registry every
// package-level function below operates on. It exists purely for source
// compatibility with the many pre-existing callers (agent, workspace, app,
// commands packages); new code that can construct its own [Registry]
// should prefer doing so.
var defaultRegistry = NewRegistry()

// ArmInit marks that MCP initialization is expected so WaitForInit blocks
// until it completes. Call this synchronously before launching Initialize in a
// goroutine; otherwise WaitForInit could observe the not-yet-started state and
// return early, letting the tool list be read before MCP tools register.
//
// ArmInit also counts a live workspace against this registry: each call must
// be matched by exactly one later Close call so the shared event broker is
// only shut down once every armed workspace has gone away (see Close and the
// liveWorkspaces doc on [Registry]). Initialize calls markInitStarted rather
// than ArmInit for this reason — app.New already calls ArmInit once per
// workspace before launching Initialize in a goroutine, and Initialize
// re-arming would double-count that single workspace against one Close.
func ArmInit() { defaultRegistry.ArmInit() }

func (r *Registry) ArmInit() {
	r.markInitStarted()
	r.refMu.Lock()
	r.liveWorkspaces++
	r.refMu.Unlock()
}

// markInitStarted records that initialization is expected, without touching
// the live-workspace refcount. See the ArmInit doc for why Initialize uses
// this instead of calling ArmInit a second time.
func (r *Registry) markInitStarted() {
	r.initMu.Lock()
	r.initStarted = true
	r.initMu.Unlock()
}

// WaitForInit blocks until MCP initialization is complete, i.e. until
// Initialize has finished and closed initDone. If initialization was never
// armed (ArmInit was not called, e.g. a coordinator built outside app
// startup), there is nothing to wait for and this returns nil immediately
// rather than blocking until ctx is cancelled.
func WaitForInit(ctx context.Context) error { return defaultRegistry.WaitForInit(ctx) }

func (r *Registry) Version() uint64 {
	return r.version.Load()
}

func (r *Registry) catalogChanged() { r.version.Add(1) }

// CatalogSnapshot is a consistent view of every advertised MCP capability.
type CatalogSnapshot struct {
	Version   uint64
	Tools     map[string][]*Tool
	Resources map[string][]*Resource
	Prompts   map[string][]*Prompt
}

func (r *Registry) CatalogSnapshot() CatalogSnapshot {
	r.catalogMu.RLock()
	defer r.catalogMu.RUnlock()
	return CatalogSnapshot{
		Version: r.version.Load(), Tools: r.allTools.Copy(),
		Resources: r.allResources.Copy(), Prompts: r.allPrompts.Copy(),
	}
}

func (r *Registry) WaitForInit(ctx context.Context) error {
	r.initMu.Lock()
	started := r.initStarted
	r.initMu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-r.initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes all MCP clients. This should be called during application
// shutdown.
//
// The shared event broker is only shut down once every workspace that armed
// this registry has closed (see the refMu/liveWorkspaces doc on [Registry]):
// a process running N workspaces against defaultRegistry must not have the
// first one to exit silently kill MCP events for the rest.
func Close(ctx context.Context) error { return defaultRegistry.Close(ctx) }

func (r *Registry) Close(ctx context.Context) error {
	r.refMu.Lock()
	if r.liveWorkspaces > 0 {
		r.liveWorkspaces--
	}
	shutdown := r.liveWorkspaces == 0
	r.refMu.Unlock()
	if !shutdown {
		return nil
	}

	// Invalidate and detach everything before any potentially blocking close.
	r.publishMu.Lock()
	if r.closing {
		r.publishMu.Unlock()
		return nil
	}
	r.closing = true
	names := map[string]struct{}{}
	for name := range r.states.Seq2() {
		names[name] = struct{}{}
	}
	for name := range r.gens.Seq2() {
		names[name] = struct{}{}
	}
	for name := range r.sessions.Seq2() {
		names[name] = struct{}{}
	}
	for name := range r.authURLs.Seq2() {
		names[name] = struct{}{}
	}
	for name := range names {
		r.gens.Set(name, r.currentGen(name)+1)
		delete(r.owners, name)
	}
	sessions := r.sessions.Copy()
	for name := range sessions {
		r.sessions.Del(name)
		delete(r.sessionOwners, name)
	}
	handlers := r.authURLs.Copy()
	for name := range handlers {
		r.authURLs.Del(name)
	}
	r.catalogMu.Lock()
	for name := range names {
		r.allTools.Del(name)
		r.allPrompts.Del(name)
		r.allResources.Del(name)
	}
	r.catalogChanged()
	r.catalogMu.Unlock()
	writeWaiters := r.tokenWriteWaitersLocked("")
	r.publishMu.Unlock()

	r.authMu.Lock()
	flows := make(map[string]*authFlow, len(r.authFlows))
	for name, flow := range r.authFlows {
		flows[name] = flow
	}
	r.authMu.Unlock()
	for name, flow := range flows {
		r.abortAuthFlow(name, flow, context.Canceled)
	}
	for _, publication := range handlers {
		publication.auth.Close()
	}

	var wg sync.WaitGroup
	for name, session := range sessions {
		wg.Go(func() { r.closeSessionContext(ctx, name, session) })
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	writeErr := waitTokenWrites(ctx, writeWaiters)
	r.broker.Shutdown()
	if writeErr != nil {
		return writeErr
	}
	return ctx.Err()
}

const lifecycleCleanupTimeout = 2 * time.Second

// SubscribeEvents returns a channel for MCP events.
//
// Every App-backed workspace now owns its own *Registry with its own
// broker (see NewRegistry / app.App.MCP), so a subscriber here only ever
// sees this registry's own servers' events — there is no cross-workspace
// fan-out to guard against anymore. Channel message events
// (EventChannelMessage) used to be filtered out here specifically because
// they carried no workspace/session identity and defaultRegistry was
// shared; that filter is gone. What still isn't wired up is delivery:
// downstream consumers (server/events.go's SSE bridge, the TUI's mcp.Event
// switch) don't yet do anything with EventChannelMessage, so it currently
// just passes through unconsumed rather than injecting into a session.
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return defaultRegistry.SubscribeEvents(ctx)
}

func (r *Registry) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return r.broker.Subscribe(ctx)
}

// GetStates returns the current state of all MCP clients.
func (r *Registry) GetStates() map[string]ClientInfo { return r.states.Copy() }

// GetState returns the state of a specific MCP client.
func GetState(name string) (ClientInfo, bool) { return defaultRegistry.GetState(name) }

func (r *Registry) GetState(name string) (ClientInfo, bool) { return r.states.Get(name) }

// Initialize initializes MCP clients based on the provided configuration.
func (r *Registry) Initialize(ctx context.Context, permissions permission.Service, cfg ConfigProvider) {
	r.markInitStarted()
	slog.Info("Initializing MCP clients")

	var wg sync.WaitGroup
	// Initialize states for all configured MCPs
	for name, m := range cfg.Config().MCP {
		if m.Disabled {
			r.updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}

		// Set initial starting state
		wg.Add(1)
		r.goInitClient(ctx, cfg, name, m, &wg)
	}
	wg.Wait()
	r.initOnce.Do(func() { close(r.initDone) })
}
