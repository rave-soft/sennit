package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
	mcpoauth "github.com/rave-soft/braid/internal/oauth/mcp"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/pubsub"
)

// Registry owns all state for a set of MCP client connections: sessions,
// per-server status, the event broker, OAuth handlers in flight, and the
// aggregated tool/prompt/resource catalogs served to the agent.
//
// Historically this state lived in package-level variables, which meant a
// single process could only ever run one MCP registry: a second workspace's
// app.New silently stomped the first's sessions, and closing either
// workspace shut down the broker for both (see ARCHITECTURE_REVIEW.md
// section 3.1). Registry makes the state instantiable so each workspace can
// (eventually) own its own. The package-level functions below still operate
// against a single [defaultRegistry] for source compatibility with existing
// callers; see init.go's refcounted Close for the stopgap that keeps that
// shared registry from tearing down other workspaces' event streams.
type Registry struct {
	sessions *csync.Map[string, *ClientSession]
	states   *csync.Map[string, ClientInfo]
	authURLs *csync.Map[string, *mcpoauth.Handler]
	broker   *pubsub.Broker[Event]

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

	// suppressMus serializes browser-suppression per server so only one
	// remote (server-driven) OAuth flow is active for a server at a time.
	suppressMus *csync.Map[string, *sync.Mutex]

	// newSession creates a client session. It is a seam so tests can exercise
	// renewal concurrency without spawning a real transport.
	newSession func(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, channelOptIn bool) (*ClientSession, error)

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
	// TODO(workspace-scoping): sessions, authURLs, and every other field
	// above are still shared (keyed only by MCP server name) across every
	// caller of the package-level wrapper functions, which all delegate to
	// defaultRegistry. Only the broker shutdown is gated here; a second
	// workspace configuring an MCP server with the same name as the first
	// still clobbers its session. Fixing that requires each workspace to own
	// its own *Registry (see ARCHITECTURE_REVIEW.md 3.1); this refcount is a
	// minimal stopgap for the broker-shutdown half of the bug.
	refMu          sync.Mutex
	liveWorkspaces int
}

// NewRegistry creates an empty, ready-to-use MCP registry.
func NewRegistry() *Registry {
	r := &Registry{
		sessions:     csync.NewMap[string, *ClientSession](),
		states:       csync.NewMap[string, ClientInfo](),
		authURLs:     csync.NewMap[string, *mcpoauth.Handler](),
		broker:       pubsub.NewBroker[Event](),
		initDone:     make(chan struct{}),
		renewMus:     map[string]*sync.Mutex{},
		gens:         csync.NewMap[string, uint64](),
		suppressMus:  csync.NewMap[string, *sync.Mutex](),
		allTools:     csync.NewMap[string, []*Tool](),
		allResources: csync.NewMap[string, []*Resource](),
		allPrompts:   csync.NewMap[string, []*Prompt](),
	}
	r.newSession = r.createSession
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
	var wg sync.WaitGroup
	for name, session := range r.sessions.Seq2() {
		wg.Go(func() {
			done := make(chan error, 1)
			go func() {
				done <- session.Close()
			}()
			select {
			case err := <-done:
				if err != nil &&
					!errors.Is(err, io.EOF) &&
					!errors.Is(err, context.Canceled) &&
					err.Error() != "signal: killed" {
					slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
				}
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
	// Clean up any remaining OAuth handlers. Like sessions above, this is
	// unconditional (not refcounted) — see the TODO on liveWorkspaces.
	for _, h := range r.authURLs.Seq2() {
		h.Close()
	}

	r.refMu.Lock()
	if r.liveWorkspaces > 0 {
		r.liveWorkspaces--
	}
	shutdown := r.liveWorkspaces <= 0
	r.refMu.Unlock()
	if shutdown {
		r.broker.Shutdown()
	}
	return nil
}

// SubscribeEvents returns a channel for MCP events.
//
// Channel message events (EventChannelMessage) are excluded: they carry no
// workspace or session identity, and defaultRegistry is shared across every
// workspace that hasn't been migrated to its own Registry. Without this
// filter, every workspace that calls SubscribeEvents would receive every
// other workspace's channel events — a cross-workspace injection path.
// Channel delivery requires workspace-scoped routing, which is deferred to a
// later PR; until then, channel events must not flow through the shared
// event fan-out.
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return defaultRegistry.SubscribeEvents(ctx)
}

func (r *Registry) SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	raw := r.broker.Subscribe(ctx)
	filtered := make(chan pubsub.Event[Event], 64)
	go func() {
		defer close(filtered)
		for ev := range raw {
			if ev.Payload.Type == EventChannelMessage {
				continue
			}
			select {
			case filtered <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return filtered
}

// GetStates returns the current state of all MCP clients.
func GetStates() map[string]ClientInfo { return defaultRegistry.GetStates() }

func (r *Registry) GetStates() map[string]ClientInfo { return r.states.Copy() }

// GetState returns the state of a specific MCP client.
func GetState(name string) (ClientInfo, bool) { return defaultRegistry.GetState(name) }

func (r *Registry) GetState(name string) (ClientInfo, bool) { return r.states.Get(name) }

// Initialize initializes MCP clients based on the provided configuration.
func Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore) {
	defaultRegistry.Initialize(ctx, permissions, cfg)
}

func (r *Registry) Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore) {
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
