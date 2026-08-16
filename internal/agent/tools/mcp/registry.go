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
// workspace shut down the broker for both (see ARCHITECTURE_REVIEW.md
// section 3.1). Every app.App now constructs and owns its own *Registry
// (app.App.MCP), so two workspaces in one process no longer share sessions,
// states, auth handlers, or the event broker. The package-level functions
// below still operate against a single [defaultRegistry] purely for source
// compatibility with tests and any caller that constructs a Registry
// directly rather than via app.New; production call sites all go through
// app.App.MCP.
type attemptID struct {
	gen uint64
	seq uint64
}

// ConfigProvider is the slice of *config.ConfigStore this package needs: the
// dictionary reads (Config, Resolver, Overrides) and the MCP OAuth token
// mutation calls. Declaring it here rather than accepting the concrete
// *config.ConfigStore keeps this package's dependency on config narrow (ISP;
// see ARCHITECTURE_REVIEW.md section S4).
type ConfigProvider interface {
	Config() *config.Config
	Resolver() config.VariableResolver
	Overrides() *config.RuntimeOverrides
	ReserveMCPTokenMutation(name string, expected config.MCPConfig) (config.MCPTokenMutation, bool)
	SetMCPToken(reservation *config.MCPTokenMutation, token *oauth.Token) (bool, error)
	ClearMCPToken(reservation *config.MCPTokenMutation, expectedToken *oauth.Token) (bool, error)
}

var _ ConfigProvider = (*config.ConfigStore)(nil)

func (a attemptID) valid() bool { return a.seq != 0 }

type tokenWriteOwner struct {
	name    string
	attempt attemptID
}

type tokenWrite struct {
	done chan struct{}
}

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

	// renewMus serializes lazy session renewals per server so concurrent tool
	// calls cannot race to rebuild the same session.
	renewMusMu sync.Mutex
	renewMus   map[string]*sync.Mutex

	// gens hands out a per-server generation number. teardown bumps a
	// server's generation; an init goroutine captures it at launch and only
	// commits its session if the generation is still current.
	gens *csync.Map[string, uint64]

	// catalogMu makes the three catalogs and their version a single snapshot.
	catalogMu sync.RWMutex
	version   atomic.Uint64

	// suppressMus serializes browser-suppression per server so only one
	// remote (server-driven) OAuth flow is active for a server at a time.
	suppressMus *csync.Map[string, *sync.Mutex]

	// publishMu serializes every externally visible per-server publication.
	// A generation check and the corresponding session, catalog, count and state
	// update must be indivisible: otherwise teardown can observe and clear an old
	// snapshot while its connector publishes it afterwards.
	publishMu     sync.Mutex
	owners        map[string]attemptID
	sessionOwners map[string]attemptID
	closing       bool

	authMu    sync.Mutex
	authFlows map[string]*authFlow

	tokenWrites        map[tokenWriteOwner]map[*tokenWrite]struct{}
	tokenReservations  map[tokenWriteOwner]*config.MCPTokenMutation
	tokenPersist       func(context.Context, ConfigProvider, string, any) error
	tokenCommit        func(ConfigProvider, *config.MCPTokenMutation, *oauth.Token) error
	beforeTokenPersist func()

	// newSession creates a client session. It is a seam so tests can exercise
	// renewal concurrency without spawning a real transport.
	newSession    func(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error)
	runAuth       func(ctx context.Context, cfg ConfigProvider, name string, m config.MCPConfig, owner attemptID) error
	ping          func(ctx context.Context, session *ClientSession, timeout time.Duration) error
	listResources func(ctx context.Context, session *ClientSession) ([]*Resource, error)

	allTools     *csync.Map[string, []*Tool]
	allResources *csync.Map[string, []*Resource]
	allPrompts   *csync.Map[string, []*Prompt]

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
		renewMus:          map[string]*sync.Mutex{},
		gens:              csync.NewMap[string, uint64](),
		suppressMus:       csync.NewMap[string, *sync.Mutex](),
		authFlows:         map[string]*authFlow{},
		tokenWrites:       map[tokenWriteOwner]map[*tokenWrite]struct{}{},
		tokenReservations: map[tokenWriteOwner]*config.MCPTokenMutation{},
		owners:            map[string]attemptID{},
		sessionOwners:     map[string]attemptID{},
		allTools:          csync.NewMap[string, []*Tool](),
		allResources:      csync.NewMap[string, []*Resource](),
		allPrompts:        csync.NewMap[string, []*Prompt](),
	}
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
// compatibility with the many pre-existing callers (agent, workspace,
// backend, ui, commands, server packages); new code that can construct its
// own [Registry] should prefer doing so.
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
// just passes through unconsumed rather than injecting into a session. See
// ARCHITECTURE_REVIEW.md section 3.1.
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
