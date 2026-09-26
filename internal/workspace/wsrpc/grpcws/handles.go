package grpcws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/rave-soft/sennit/internal/workspace"
)

// clientMetadataKey is the outgoing metadata key a Client attaches to
// every call, naming which lease it belongs to (CLIENT-SERVER.md, PR 1.3):
// the handles a client's own EnterWorktree/ExitWorktree/AttachThread calls
// register are released once this client has gone quiet -- no open RPC or
// stream -- for longer than the server's grace period (see leaseManager).
const clientMetadataKey = "sennit-client"

// handlesServiceName is the fourth hand-written service (alongside Meta,
// Events and Agent): EnterWorktree/ExitWorktree/AttachThread are class H
// (CLIENT-SERVER.md's method-class table) -- they hand back another
// Workspace rather than plain data, so, like the other non-U/C classes,
// the generator emits nothing for them.
const handlesServiceName = "sennit.workspace.v1.Handles"

// EnterWorktreeRequest is EnterWorktree's request.
type EnterWorktreeRequest struct {
	Name string `json:"name"`
}

// ExitWorktreeRequest is ExitWorktree's (empty) request.
type ExitWorktreeRequest struct{}

// AttachThreadRequest is AttachThread's request.
type AttachThreadRequest struct {
	ID string `json:"id"`
}

// HandleResponse is what EnterWorktree/ExitWorktree/AttachThread hand
// back on success: Handle names the newly registered workspace for every
// later call's "sennit-handle" metadata (see handleFromContext),
// WorkingDir is that workspace's own WorkingDir() at the moment it was
// registered.
type HandleResponse struct {
	Handle     string `json:"handle"`
	WorkingDir string `json:"working_dir"`
}

// ReleaseHandleRequest is ReleaseHandle's request: the handle to release,
// exactly as HandleResponse handed it back.
type ReleaseHandleRequest struct {
	Handle string `json:"handle"`
}

// ReleaseHandleResponse is ReleaseHandle's (empty) response.
type ReleaseHandleResponse struct{}

// HandlesServer is the interface grpc.Server.RegisterService checks the
// registered handler against (see MetaServer's doc comment for why this
// can't just be *handlesServer).
type HandlesServer interface {
	EnterWorktree(ctx context.Context, req *EnterWorktreeRequest) (*HandleResponse, error)
	ExitWorktree(ctx context.Context, req *ExitWorktreeRequest) (*HandleResponse, error)
	AttachThread(ctx context.Context, req *AttachThreadRequest) (*HandleResponse, error)
	ReleaseHandle(ctx context.Context, req *ReleaseHandleRequest) (*ReleaseHandleResponse, error)
}

var handlesServiceDesc = grpc.ServiceDesc{
	ServiceName: handlesServiceName,
	HandlerType: (*HandlesServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "EnterWorktree", Handler: _Handles_EnterWorktree_Handler},
		{MethodName: "ExitWorktree", Handler: _Handles_ExitWorktree_Handler},
		{MethodName: "AttachThread", Handler: _Handles_AttachThread_Handler},
		{MethodName: "ReleaseHandle", Handler: _Handles_ReleaseHandle_Handler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "wsrpc/handles",
}

// handlesUnaryHandler builds a grpc.MethodDesc.Handler for one HandlesServer
// method, following the same decode/interceptor/dispatch shape
// _Meta_Hello_Handler uses -- kept as a small generic instead of four
// hand-copied bodies, since (unlike Meta/Agent/Events, each with a single
// method or a fixed pair of request/response types) this service has four
// methods that would otherwise repeat the same boilerplate four times.
func handlesUnaryHandler[Req, Resp any](methodName string, call func(*handlesServer, context.Context, *Req) (*Resp, error)) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		in := new(Req)
		if err := dec(in); err != nil {
			return nil, err
		}
		s := srv.(*handlesServer)
		if interceptor == nil {
			return call(s, ctx, in)
		}
		info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + handlesServiceName + "/" + methodName}
		handler := func(ctx context.Context, req any) (any, error) {
			return call(s, ctx, req.(*Req))
		}
		return interceptor(ctx, in, info, handler)
	}
}

var _Handles_EnterWorktree_Handler = handlesUnaryHandler("EnterWorktree", (*handlesServer).EnterWorktree)

var _Handles_ExitWorktree_Handler = handlesUnaryHandler("ExitWorktree", (*handlesServer).ExitWorktree)

var _Handles_AttachThread_Handler = handlesUnaryHandler("AttachThread", (*handlesServer).AttachThread)

var _Handles_ReleaseHandle_Handler = handlesUnaryHandler("ReleaseHandle", (*handlesServer).ReleaseHandle)

// handlesServer adapts the handle registry to HandlesServer: resolve
// mirrors workspaceServer/agentServer's own (see zz_generated_service.go),
// finding the workspace this call's own "sennit-handle" metadata already
// names -- EnterWorktree/ExitWorktree/AttachThread all act on "whatever
// workspace this client is currently viewing", the same as any other
// Workspace method, not on the handle they're about to mint.
type handlesServer struct {
	resolve  func(ctx context.Context) (workspace.Workspace, error)
	registry *handleRegistry
}

func (s *handlesServer) EnterWorktree(ctx context.Context, req *EnterWorktreeRequest) (*HandleResponse, error) {
	ws, err := s.resolve(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	child, release, err := ws.EnterWorktree(ctx, req.Name)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	return s.register(ctx, child, release), nil
}

func (s *handlesServer) ExitWorktree(ctx context.Context, _ *ExitWorktreeRequest) (*HandleResponse, error) {
	ws, err := s.resolve(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	child, release, err := ws.ExitWorktree(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	return s.register(ctx, child, release), nil
}

func (s *handlesServer) AttachThread(ctx context.Context, req *AttachThreadRequest) (*HandleResponse, error) {
	ws, err := s.resolve(ctx)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	child, release, err := ws.AttachThread(ctx, req.ID)
	if err != nil {
		return nil, grpcStatusFromError(ctx, err)
	}
	return s.register(ctx, child, release), nil
}

// ReleaseHandle drops req.Handle from the registry, running its stored
// release func (and stopping its event hub) exactly once -- idempotent, so
// a client that races this against the lease's own grace-expiry sweep (or
// simply calls it twice) never double-releases (CLIENT-SERVER.md, PR 1.3).
// An unknown handle is not an error: it may already be gone by either
// path, and the caller's intent ("I'm done with this handle") is
// satisfied either way.
func (s *handlesServer) ReleaseHandle(_ context.Context, req *ReleaseHandleRequest) (*ReleaseHandleResponse, error) {
	s.registry.release(req.Handle)
	return &ReleaseHandleResponse{}, nil
}

// register records ws (the workspace an H method just returned) in the
// registry under a fresh handle, owned by this call's client lease, and
// builds the response the wire contract promises.
func (s *handlesServer) register(ctx context.Context, ws workspace.Workspace, release func()) *HandleResponse {
	owner := clientIDFromContext(ctx)
	handle := s.registry.register(ws, release, owner)
	return &HandleResponse{Handle: handle, WorkingDir: ws.WorkingDir()}
}

// clientIDFromContext reads the "sennit-client" metadata key an incoming
// call carries (see Client's clientID), or "" if absent -- a caller not
// using this package's own Client (a hand-rolled grpc client, say) owns no
// lease, so its handles (if it minted any before losing interest) are
// never swept by leaseManager; NewServer's registry cleanup on Stop is
// what still catches those.
// ClientIDFromContext exports clientIDFromContext for a hand-written
// service handler (internal/daemon's Shutdown handler, see
// WithShutdownHandler) that needs to know which client is making the
// current call -- typically to exclude that call's own, necessarily-open
// connection from a busyness check it is about to run.
func ClientIDFromContext(ctx context.Context) string {
	return clientIDFromContext(ctx)
}

func clientIDFromContext(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get(clientMetadataKey)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// handleEntry is one registered non-root workspace: the workspace itself,
// its own event hub (lazily started, like the root's -- see eventHub.
// ensureStarted), which client leases it, and the release func its own H
// method returned (EnterWorktree/ExitWorktree's app teardown, or
// AttachThread's no-op detach -- see workspace.ThreadController.
// AttachThread's doc comment on why detaching never stops the thread).
type handleEntry struct {
	ws      workspace.Workspace
	hub     *eventHub
	owner   string
	once    sync.Once
	release func()
}

// handleRegistry maps handle IDs to the workspace.Workspace each names,
// for every handle EnterWorktree/ExitWorktree/AttachThread has minted and
// not yet released (CLIENT-SERVER.md, PR 1.3). The root handle ("") is
// never in here -- it lives for the server's whole lifetime and is
// resolved directly by NewServer's own closures.
type handleRegistry struct {
	eventBufferSize       int
	clientStateTickPeriod time.Duration

	mu   sync.Mutex
	byID map[string]*handleEntry
}

func newHandleRegistry(eventBufferSize int, clientStateTickPeriod time.Duration) *handleRegistry {
	return &handleRegistry{
		eventBufferSize: eventBufferSize, clientStateTickPeriod: clientStateTickPeriod,
		byID: map[string]*handleEntry{},
	}
}

// register mints a fresh, unguessable handle ID for ws and stores it,
// owned by owner (a client ID, or "" for a caller with no lease -- see
// clientIDFromContext). Every registered handle gets its own event hub,
// started right away (CLIENT-SERVER.md, PR 1.4a: "per-handle hubs still
// start when the handle is registered", the same eager timing NewServer
// now gives the root hub) rather than lazily on the handle's first
// Events.Subscribe call, so its client-state publisher is already running
// -- a Snapshot RPC against this handle has something to report from the
// moment it exists.
func (r *handleRegistry) register(ws workspace.Workspace, release func(), owner string) string {
	id := randomToken()
	hub := newEventHubWithStateTick(r.eventBufferSize, r.clientStateTickPeriod)
	// ws is nil in a handful of registry-only unit tests that never touch
	// the hub; every production H method (EnterWorktree/ExitWorktree/
	// AttachThread) hands back a real workspace.Workspace on success, so
	// this guard never fires there.
	if ws != nil {
		hub.ensureStarted(ws)
	}
	entry := &handleEntry{ws: ws, hub: hub, owner: owner, release: release}
	r.mu.Lock()
	r.byID[id] = entry
	r.mu.Unlock()
	return id
}

// resolve looks up handle's workspace.Workspace, or workspace.ErrWorkspaceGone
// if it's unknown -- released already, expired by its owner's lease grace,
// or never minted. ErrWorkspaceGone is an existing sentinel (see
// workspace.go's doc comment: "the server is reachable but no longer knows
// this client's workspace"), reused here rather than a new one: that is
// exactly this situation, and EncodeError/DecodeError already round-trip
// it with a dedicated wire code ("workspace_gone"), so a client can tell a
// gone handle apart from any other NotFound.
func (r *handleRegistry) resolve(handle string) (workspace.Workspace, error) {
	r.mu.Lock()
	entry, ok := r.byID[handle]
	r.mu.Unlock()
	if !ok {
		return nil, workspace.ErrWorkspaceGone
	}
	return entry.ws, nil
}

// resolveHub is resolve's counterpart for the Events service: it also
// lazily starts handle's own upstream subscription (ensureStarted is a
// no-op past its first call) before handing the hub back.
func (r *handleRegistry) resolveHub(handle string) (*eventHub, error) {
	r.mu.Lock()
	entry, ok := r.byID[handle]
	r.mu.Unlock()
	if !ok {
		return nil, workspace.ErrWorkspaceGone
	}
	entry.hub.ensureStarted(entry.ws)
	return entry.hub, nil
}

// release drops handle from the registry (so a concurrent resolve/
// resolveHub sees it gone immediately) and runs its hub.close/release
// exactly once, in that order: closing the hub first stops this handle's
// own event subscription before release tears down whatever backs it, so
// ensureStarted's upstream ws.SubscribeWith isn't left reading from an app
// that's already gone. Reports whether handle was actually found (a
// caller that only wants "make sure it's gone" — ReleaseHandle, the lease
// sweep — doesn't need this, but it's what makes the return value useful
// for a test to assert on).
func (r *handleRegistry) release(handle string) bool {
	r.mu.Lock()
	entry, ok := r.byID[handle]
	if ok {
		delete(r.byID, handle)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	entry.once.Do(func() {
		entry.hub.close()
		entry.release()
	})
	return true
}

// releaseByOwner releases every handle currently owned by owner -- the
// lease sweep's action once a client has gone quiet past the grace period
// (CLIENT-SERVER.md, PR 1.3).
func (r *handleRegistry) releaseByOwner(owner string) {
	r.mu.Lock()
	var ids []string
	for id, entry := range r.byID {
		if entry.owner == owner {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.release(id)
	}
}

// closeAll releases every handle still registered, regardless of owner --
// NewServer's returned stop func calls this alongside the root hub's own
// close, so stopping the server leaves no handle's hub goroutine running
// (see NewServer's doc comment on its stop func).
func (r *handleRegistry) closeAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		r.release(id)
	}
}

// randomToken mints a 128-bit unguessable string: both a handle ID (this
// file) and a Client's default lease ID (client_manual.go's NewClient) are
// opaque bearer tokens -- whoever holds one can act as its owner (see
// NewServer's doc comment on auth being out of scope) -- so neither must
// be predictable the way a counter would be.
func randomToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand.Read on any of Go's supported platforms only fails
		// if the OS's own entropy source is broken -- there is no sane
		// fallback to a "less random" ID at that point, so this mirrors
		// how the stdlib itself treats the same failure (e.g.
		// crypto/rand's own package doc: "the output is suitable for
		// keying permanent... secrets", i.e. this is not meant to be
		// recoverable).
		panic("wsrpc: reading random bytes for a token: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

// defaultHandleLeaseGrace is how long a client may go with no open RPC or
// stream before its handles are released (CLIENT-SERVER.md, PR 1.3),
// matching the old supervisor's own detach grace
// (027d6155c^:internal/backend/backend.go).
const defaultHandleLeaseGrace = 10 * time.Second

// clientLease is one client ID's current activity: active counts the RPCs
// and streams presently open for it: while it's positive, this client is
// not idle, so a stale grace timer from an earlier idle period is
// cancelled. Once active drops back to zero, timer is armed for grace;
// reaching zero and staying there for the whole grace period is what
// triggers the release.
type clientLease struct {
	active int
	timer  *time.Timer
}

// leaseManager ties a client's connection activity (every unary call and
// every open stream, tracked by its own unary/stream interceptor) to the
// handles it owns: once a client has had no open RPC or stream for grace,
// every handle it holds is released, so a client that never reconnects
// doesn't pin worktree/thread views (and their App instances) open
// forever. A client that reconnects within grace -- a new connection,
// same client ID -- keeps its handles, because the timer backing that
// release is cancelled by the next call's begin() before it ever fires
// (CLIENT-SERVER.md, PR 1.3). oauthRegistry gets the same sweep, for the
// same owner, as registry: a client that goes quiet mid sign-in must not
// leave its OAuthFlow (and whatever resource it holds -- a loopback
// listener, an in-flight poll) pinned open any more than its worktree/
// thread handles are (PR 1.3b-2).
type leaseManager struct {
	grace         time.Duration
	registry      *handleRegistry
	oauthRegistry *oauthFlowRegistry

	mu      sync.Mutex
	clients map[string]*clientLease
}

func newLeaseManager(grace time.Duration, registry *handleRegistry, oauthRegistry *oauthFlowRegistry) *leaseManager {
	if grace <= 0 {
		grace = defaultHandleLeaseGrace
	}
	return &leaseManager{grace: grace, registry: registry, oauthRegistry: oauthRegistry, clients: map[string]*clientLease{}}
}

// begin marks clientID as having one more open RPC/stream, cancelling any
// grace timer running for it -- a client mid-call, by definition, isn't
// idle.
func (lm *leaseManager) begin(clientID string) {
	if clientID == "" {
		return
	}
	lm.mu.Lock()
	defer lm.mu.Unlock()
	cl, ok := lm.clients[clientID]
	if !ok {
		cl = &clientLease{}
		lm.clients[clientID] = cl
	}
	cl.active++
	if cl.timer != nil {
		cl.timer.Stop()
		cl.timer = nil
	}
}

// end reports one of clientID's RPCs/streams as finished. Once active
// reaches zero, a grace timer is armed; if nothing calls begin again
// before it fires, expire runs.
func (lm *leaseManager) end(clientID string) {
	if clientID == "" {
		return
	}
	lm.mu.Lock()
	defer lm.mu.Unlock()
	cl, ok := lm.clients[clientID]
	if !ok {
		return
	}
	cl.active--
	if cl.active > 0 {
		return
	}
	cl.timer = time.AfterFunc(lm.grace, func() { lm.expire(clientID) })
}

// expire releases every handle clientID owns, unless a new call arrived
// (and thus a new begin bumped active back up) in the window between the
// timer firing and this running.
func (lm *leaseManager) expire(clientID string) {
	lm.mu.Lock()
	cl, ok := lm.clients[clientID]
	if !ok || cl.active > 0 {
		lm.mu.Unlock()
		return
	}
	delete(lm.clients, clientID)
	lm.mu.Unlock()
	lm.registry.releaseByOwner(clientID)
	lm.oauthRegistry.releaseByOwner(clientID)
}

// clientCount reports how many clients currently have an entry in
// lm.clients: an open RPC/stream, or a lease still within its post-hangup
// grace period. A client whose grace timer has fired (expire has already
// deleted it) is not counted.
func (lm *leaseManager) clientCount() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return len(lm.clients)
}

// clientCountExcluding is clientCount but never counts excludeID. The
// Shutdown RPC's OnlyIfIdle check (internal/daemon) uses this rather than
// clientCount: begin runs before the handler that decides busyness even
// executes (see unaryInterceptor), so the very call asking "is anyone
// using this daemon" would otherwise always count itself as a connected
// client and the check could never say yes.
func (lm *leaseManager) clientCountExcluding(excludeID string) int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	n := len(lm.clients)
	if excludeID != "" {
		if _, ok := lm.clients[excludeID]; ok {
			n--
		}
	}
	return n
}

// unaryInterceptor is the leaseManager's half of a grpc.ChainUnaryInterceptor
// option (see NewServer): every unary call, across every service this
// server registers, counts as activity for its "sennit-client" metadata,
// for exactly its own duration.
func (lm *leaseManager) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	clientID := clientIDFromContext(ctx)
	lm.begin(clientID)
	defer lm.end(clientID)
	return handler(ctx, req)
}

// streamInterceptor is unaryInterceptor's counterpart for
// grpc.ChainStreamInterceptor: a stream (Events.Subscribe, Agent.
// AgentRunStream/AgentRunShellCommand) counts as activity for its whole
// lifetime, not just while it's actively sending -- a long-lived
// subscription is exactly the opposite of an idle client.
func (lm *leaseManager) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	clientID := clientIDFromContext(ss.Context())
	lm.begin(clientID)
	defer lm.end(clientID)
	return handler(srv, ss)
}
