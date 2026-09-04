package permission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
}

type PermissionNotification struct {
	ToolCallID string `json:"tool_call_id"`
	Granted    bool   `json:"granted"`
	Denied     bool   `json:"denied"`
}

type PermissionRequest struct {
	ID          string `json:"id"`
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
	// Delegation identifies the background delegation whose run raised
	// this request (see [WithDelegation]), or the zero value if the
	// visible turn asked.
	Delegation DelegationRef `json:"-"`
}

type permissionRequestJSON struct {
	ID          string         `json:"id"`
	SessionID   string         `json:"session_id"`
	ToolCallID  string         `json:"tool_call_id"`
	ToolName    string         `json:"tool_name"`
	Description string         `json:"description"`
	Action      string         `json:"action"`
	Params      any            `json:"params"`
	Path        string         `json:"path"`
	Delegation  *DelegationRef `json:"delegation,omitempty"`
}

func (p PermissionRequest) MarshalJSON() ([]byte, error) {
	request := permissionRequestJSON{
		ID:          p.ID,
		SessionID:   p.SessionID,
		ToolCallID:  p.ToolCallID,
		ToolName:    p.ToolName,
		Description: p.Description,
		Action:      p.Action,
		Params:      p.Params,
		Path:        p.Path,
	}
	if p.Delegation != (DelegationRef{}) {
		request.Delegation = &p.Delegation
	}
	return json.Marshal(request)
}

func (p *PermissionRequest) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}

	delegation := p.Delegation
	request := permissionRequestJSON{
		ID:          p.ID,
		SessionID:   p.SessionID,
		ToolCallID:  p.ToolCallID,
		ToolName:    p.ToolName,
		Description: p.Description,
		Action:      p.Action,
		Params:      p.Params,
		Path:        p.Path,
		Delegation:  &delegation,
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return err
	}

	p.ID = request.ID
	p.SessionID = request.SessionID
	p.ToolCallID = request.ToolCallID
	p.ToolName = request.ToolName
	p.Description = request.Description
	p.Action = request.Action
	p.Params = request.Params
	p.Path = request.Path
	p.Delegation = DelegationRef{}
	if request.Delegation != nil {
		p.Delegation = *request.Delegation
	}
	return nil
}

// Requester is the asking side of the permission service: tools and the
// agent, which need to raise a request and, for file-shaped tools, check
// the confinement boundary before they even do so.
//
// ConfinedDir lives here rather than on Resolver because it isn't part of
// asking — it's what a confined workspace's file-shaped tools consult
// *before* asking, to refuse outright rather than prompt when a request
// would escape the workspace (see internal/agent/tools/confinement.go and
// bashConfinementRefusal in internal/agent/tools/bash.go). Asking the user
// to approve an escape would be the wrong question.
type Requester interface {
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	// ConfinedDir returns the directory this workspace's writes are
	// confined to, or "" when they are not confined.
	//
	// This is a boundary, not a permission: a confined workspace is one
	// whose whole purpose is to keep its changes to itself — a thread,
	// working in its own git worktree on its own branch. Asking the user
	// to approve an escape would be the wrong question, and under yolo
	// (which threads inherit from the main agent) nobody would be asked
	// at all. So it is enforced ahead of the permission flow rather than
	// through it; see the file tools, which refuse rather than prompt.
	ConfinedDir() string
}

// Resolver is the answering side of the permission service: the
// permission dialog UI and AppWorkspace, which settle a pending request
// one way or another.
type Resolver interface {
	// GrantPersistent grants a permission request and remembers the grant
	// for the session. It returns true if this call actually resolved the
	// pending request; false if the request had already been resolved
	// (e.g., by another concurrent caller) or is unknown.
	GrantPersistent(permission PermissionRequest) bool
	// Grant grants a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Grant(permission PermissionRequest) bool
	// Deny denies a permission request. It returns true if this call
	// actually resolved the pending request; false if the request had
	// already been resolved or is unknown.
	Deny(permission PermissionRequest) bool
	// ActiveRequest returns the request currently awaiting an answer,
	// for subscribers that need to recover one they did not see
	// published. See the implementation for why this is needed at all.
	ActiveRequest() (PermissionRequest, bool)
}

// Observer is the watching side of the permission service: the UI and the
// delegation idle watchdog (internal/thread), which need to know what is
// pending without themselves asking or answering.
type Observer interface {
	pubsub.Subscriber[PermissionRequest]
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	// AwaitingAnswer reports whether a request raised by the delegation
	// identified by delegationID is outstanding right now — either shown
	// to the user or queued behind another request.
	//
	// It exists for the delegation idle watchdog (internal/thread), which
	// ends a background run that has stopped producing anything. A run
	// blocked on a permission prompt has stopped producing anything and
	// is nonetheless perfectly healthy: it is waiting for a person, which
	// is a wait with no upper bound by design (see the note on
	// [Service.Request] blocking). Without this the watchdog would kill
	// exactly the delegations the user was about to approve.
	AwaitingAnswer(delegationID string) bool
	SkipRequests() bool
}

// Controller holds the session- and workspace-level switches on the
// permission service: auto-approving a session, skipping requests
// entirely, and confining a workspace to its working directory.
type Controller interface {
	AutoApproveSession(sessionID string)
	// IsAutoApproveSession reports whether sessionID was auto-approved by
	// AutoApproveSession. A delegation's launch site uses this to decide
	// whether its child session inherits the grant: a child started under
	// an auto-approved parent gets nothing a plain AutoApproveSession(child)
	// wouldn't already give it, since the parent already approves
	// everything; a child started under an ordinary session must still
	// prompt.
	IsAutoApproveSession(sessionID string) bool
	SetSkipRequests(skip bool)
	// ConfineToWorkingDir marks this workspace as one that may not write
	// outside its working directory at all. See Requester.ConfinedDir.
	ConfineToWorkingDir()
}

type Service interface {
	Requester
	Resolver
	Observer
	Controller
}

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
	// Params scopes persistent grants to the exact operation parameters.
	// File paths alone are intentionally insufficient for operations such as
	// symbol rename, where a second target or replacement is a distinct write.
	Params string
}

type permissionService struct {
	// confined marks this service's workspace as write-confined to
	// workingDir; see Service.ConfinedDir.
	confined atomic.Bool

	*pubsub.Broker[PermissionRequest]

	notificationBroker    *pubsub.Broker[PermissionNotification]
	workingDir            string
	sessionPermissions    *csync.Map[PermissionKey, bool]
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  atomic.Bool
	allowedTools          []string

	// dialogMu guards current and queue below, which together implement
	// a one-at-a-time dispatch of permission requests to the UI. The UI
	// relies on exactly one PermissionRequest event being outstanding
	// at any time (see openPermissionsDialog in internal/ui), so a
	// second request must wait to be published until the first is
	// resolved. This lock is only ever held for the brief bookkeeping
	// below, never across a Request call's wait for a user response.
	dialogMu sync.Mutex
	// current is the ID of the request currently published to the UI,
	// or "" if none is outstanding.
	current string
	// currentReq is the request current identifies, retained so a
	// subscriber that missed its publish can recover it - see
	// ActiveRequest. Publishing a request is the only announcement it
	// ever gets, and a missed one blocks its caller forever, so the
	// value has to stay readable after the event has gone by.
	currentReq PermissionRequest
	// queue holds requests that have arrived while another request is
	// current, in FIFO order.
	queue []PermissionRequest
}

// resolve atomically removes the pending request entry for the given
// permission and, if it was still pending, publishes exactly one
// PermissionNotification and forwards the outcome to the waiter on
// respCh. It returns true if this call resolved the request, false if
// it had already been resolved (e.g., by another concurrent caller) or
// the request ID is unknown.
//
// If onResolve is non-nil it runs after the pending entry has been
// taken but before the notification is published or the waiter is
// unblocked. This lets GrantPersistent record the session permission
// only when it actually wins the race, so a losing GrantPersistent
// that lost to a Deny does not leak an auto-approve entry.
//
// All three public resolution methods (Grant, GrantPersistent, Deny)
// route through this helper so multi-subscriber UIs can race safely:
// the first caller wins, the rest become no-ops.
func (s *permissionService) resolve(permission PermissionRequest, granted, denied bool, onResolve func()) bool {
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		return false
	}

	if onResolve != nil {
		onResolve()
	}

	s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    granted,
		Denied:     denied,
	})

	// respCh is buffered (cap 1) and only ever has at most one sender
	// per request because Take removes the entry under the map lock,
	// so this send never blocks.
	respCh <- granted

	// This request no longer occupies the "currently shown" slot (if it
	// ever did); let the next queued request take its place.
	s.dispatchNext(permission.ID)

	return true
}

// enqueue publishes the request immediately if no other request is
// currently awaiting a response, otherwise it inserts it into the queue
// (see insertQueued) to be published once the current one resolves. This
// is what guarantees the UI only ever sees one PermissionRequest event
// outstanding at a time. current itself is never touched here: whatever
// is already on screen stays on screen until it resolves or its ctx ends,
// regardless of what arrives behind it.
func (s *permissionService) enqueue(permission PermissionRequest) {
	s.dialogMu.Lock()
	if s.current == "" {
		s.current = permission.ID
		s.currentReq = permission
		s.dialogMu.Unlock()
		s.publishRequest(permission)
		return
	}
	s.insertQueued(permission)
	s.dialogMu.Unlock()
}

// publishRequest announces a request to whoever is watching, with
// bounded-blocking delivery rather than the lossy default.
//
// A dropped request is not a missed update that the next one corrects:
// this publish is the request's only announcement, and its caller is
// already blocked in Request waiting for an answer that now cannot come.
// The tool call hangs with nothing on screen to explain it.
//
// Bounded-blocking still permits a drop after the per-subscriber timeout,
// which is why ActiveRequest exists: delivery is made as reliable as the
// broker allows, and what is left over is made recoverable.
//
// The context is deliberately not the requester's: this is called from
// Request (whose ctx may end at any moment) and from dispatchNext on the
// cancellation path, where the ctx that reached it is already done.
// Handing either to a bounded-blocking publish would abandon the fan-out
// immediately and lose exactly the event this is here to deliver.
func (s *permissionService) publishRequest(permission PermissionRequest) {
	s.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, permission)
}

// insertQueued inserts permission into s.queue so that every foreground
// request (Delegation is the zero value — the visible turn) sits ahead of
// every background one (raised by a delegation, e.g. a task): a user
// continuing their own conversation should not have to answer for queued
// background work first. Within each class, order is FIFO — insertion
// never reorders two requests already in the same class, so this stays
// stable. dispatchNext always pops queue[0], so keeping the queue sorted
// here is what makes that pop correct; must be called with dialogMu held.
func (s *permissionService) insertQueued(permission PermissionRequest) {
	if permission.Delegation != (DelegationRef{}) {
		// Background: goes behind every request already queued, whether
		// foreground or background — this is what keeps background
		// requests FIFO among themselves without needing to hunt for
		// where the background run of the queue starts.
		s.queue = append(s.queue, permission)
		return
	}
	// Foreground: goes behind every foreground request already queued
	// (FIFO among foreground requests) but ahead of every background one,
	// wherever those currently sit.
	i := 0
	for i < len(s.queue) && s.queue[i].Delegation == (DelegationRef{}) {
		i++
	}
	s.queue = slices.Insert(s.queue, i, permission)
}

// dispatchNext is called once a request identified by id has been finally
// settled (resolved via Grant/Deny/GrantPersistent, or canceled via ctx).
// If id was the request currently shown to the UI, the next queued request
// (if any) is published and becomes current. If id was still waiting in
// the queue (e.g. resolved or canceled before ever being shown), it is
// simply removed. Calling this more than once for the same id is safe: by
// the time a second call arrives, id is neither current nor in the queue,
// so both branches are no-ops.
func (s *permissionService) dispatchNext(id string) {
	s.dialogMu.Lock()
	if s.current != id {
		s.queue = slices.DeleteFunc(s.queue, func(p PermissionRequest) bool {
			return p.ID == id
		})
		s.dialogMu.Unlock()
		return
	}

	if len(s.queue) == 0 {
		s.current = ""
		s.currentReq = PermissionRequest{}
		s.dialogMu.Unlock()
		return
	}

	next := s.queue[0]
	s.queue = s.queue[1:]
	s.current = next.ID
	s.currentReq = next
	s.dialogMu.Unlock()
	s.publishRequest(next)
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) bool {
	// Record the persistent grant only if this call wins the
	// pending-request race. Otherwise a losing GrantPersistent that
	// lost to a Deny would still leave an auto-approve entry behind,
	// silently flipping later denied calls to allowed.
	return s.resolve(permission, true, false, func() {
		s.sessionPermissions.Set(PermissionKey{
			SessionID: permission.SessionID,
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
			Params:    permissionParamsKey(permission.Params),
		}, true)
	})
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	return s.resolve(permission, true, false, nil)
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	return s.resolve(permission, false, true, nil)
}

// KnownActions is the vocabulary CreatePermissionRequest.Action draws
// from, and therefore the only second half an allowlist entry of the form
// "tool:action" can have. It is a closed set on purpose: Request builds
// its lookup key by joining the tool name and the action, so an entry
// naming anything else - a command line, say - can never match, grants
// nothing, and leaves the person answering a prompt they believe they
// turned off. config.Doctor checks entries against this list;
// TestPermissionActionsAreKnown keeps the list and the tools that raise
// requests from drifting apart.
var KnownActions = []string{
	"cancel",
	"create",
	"download",
	"execute",
	"fetch",
	"list",
	"merge",
	"read",
	"remove",
	"rename",
	"search",
	"write",
}

// IsKnownAction reports whether action is one of KnownActions.
func IsKnownAction(action string) bool {
	return slices.Contains(KnownActions, action)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist. The
	// action half is one of KnownActions, never free text: an allowlist
	// entry of "bash:npm run build" matches nothing and grants nothing.
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{ // ok: detached - the outcome must reach the UI even if the caller's context dies mid-publish
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	// tell the UI that a permission was requested
	s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{ // ok: detached - as above
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{ // ok: detached - as above
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	// A relative path is relative to the workspace, not to the process
	// cwd — resolving it before it becomes the persistent-grant key keeps
	// that key canonical, so a grant recorded for a relative spelling
	// matches the later absolute request for the same file (and never a
	// same-named file elsewhere). An absolute path is Cleaned for the same
	// reason: "/a/b/../c" and "/a/c" must key to the same grant.
	//
	// The key is the path itself, not its containing directory: it must
	// not depend on whether the path happens to exist on disk right now
	// (a Stat-based check here previously widened every file grant to its
	// whole directory, and did so specifically to dodge the disagreement
	// an existence check produces between a request for a file that does
	// not exist yet and the next request for the same, now-created file —
	// see the tests for that regression). A directory-shaped tool (bash, ls)
	// still passes its working directory in opts.Path, so its key is
	// still that directory; only file-shaped tools get a narrower grant.
	path := opts.Path
	if path == "" || path == "." {
		path = s.workingDir
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(s.workingDir, path)
	} else {
		path = filepath.Clean(path)
	}
	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        path,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
		Delegation:  DelegationFromContext(ctx),
	}

	if _, ok := s.sessionPermissions.Get(PermissionKey{
		SessionID: permission.SessionID,
		ToolName:  permission.ToolName,
		Action:    permission.Action,
		Path:      permission.Path,
		Params:    permissionParamsKey(permission.Params),
	}); ok {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{ // ok: detached - as above
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)

	// Publish the request now if no other request is being shown to the
	// user, otherwise queue it to be published once the current one is
	// resolved. Request no longer holds a lock across this wait: the
	// dispatch above (and dispatchNext, called from resolve/cancel) is
	// what serializes what the UI sees, not this goroutine blocking.
	s.enqueue(permission)

	select {
	case <-ctx.Done():
		// Only the goroutine that wins the Take is responsible for
		// advancing the queue: if Grant/Deny raced us and already took
		// the entry, resolve has already called dispatchNext and this
		// is a safe no-op.
		if _, ok := s.pendingRequests.Take(permission.ID); ok {
			// Cancellation settles a request just like an explicit denial.
			// Without this terminal notification, the UI keeps its dialog
			// open after the requester has gone away.
			s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{ // ok: detached - this is the cancellation path: the context that would carry it is the one that just died, and without this the dialog stays open
				ToolCallID: permission.ToolCallID,
				Denied:     true,
			})
			s.dispatchNext(permission.ID)
		}
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

// permissionParamsKey converts request parameters into a stable, comparable
// key for session grants. JSON's deterministic map-key ordering makes this
// safe for both typed tool params and map-shaped requests. If a caller passes
// an unsupported value, fail closed by giving it a unique empty key only when
// it is nil; otherwise use its type so it cannot accidentally match a valid
// serialized request.
func permissionParamsKey(params any) string {
	if params == nil {
		return ""
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Sprintf("invalid:%T", params)
	}
	return string(encoded)
}

// ActiveRequest returns the request currently awaiting an answer, if
// any. It is the recovery path for a subscriber that was not listening
// when the request was published, or whose delivery was dropped after
// the bounded-blocking timeout.
//
// This matters because a permission request is announced exactly once
// and its caller blocks on the answer indefinitely. Every other event in
// this system is corrected by the next one; a lost request is corrected
// by nothing, and shows up as work that stopped for no visible reason.
func (s *permissionService) ActiveRequest() (PermissionRequest, bool) {
	s.dialogMu.Lock()
	defer s.dialogMu.Unlock()
	if s.current == "" {
		return PermissionRequest{}, false
	}
	return s.currentReq, true
}

// AwaitingAnswer reports whether delegationID has a request outstanding.
// Both halves of the one-at-a-time dispatch count: the request currently
// shown, and every one still queued behind it — a delegation waiting for
// its turn at the dialog is just as blocked as the one holding it.
//
// Read under dialogMu, the same lock that maintains both, so the answer
// is a consistent snapshot rather than two reads a dispatch could slip
// between.
func (s *permissionService) AwaitingAnswer(delegationID string) bool {
	if delegationID == "" {
		return false
	}
	s.dialogMu.Lock()
	defer s.dialogMu.Unlock()
	if s.current != "" && s.currentReq.Delegation.ID == delegationID {
		return true
	}
	return slices.ContainsFunc(s.queue, func(p PermissionRequest) bool {
		return p.Delegation.ID == delegationID
	})
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
}

func (s *permissionService) IsAutoApproveSession(sessionID string) bool {
	s.autoApproveSessionsMu.RLock()
	defer s.autoApproveSessionsMu.RUnlock()
	return s.autoApproveSessions[sessionID]
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

// ConfineToWorkingDir implements Service.
func (s *permissionService) ConfineToWorkingDir() { s.confined.Store(true) }

// ConfinedDir implements Service.
func (s *permissionService) ConfinedDir() string {
	if !s.confined.Load() {
		return ""
	}
	return s.workingDir
}

func NewPermissionService(workingDir string, skip bool, allowedTools []string) Service {
	svc := &permissionService{
		Broker:              pubsub.NewBroker[PermissionRequest](),
		notificationBroker:  pubsub.NewBroker[PermissionNotification](),
		workingDir:          workingDir,
		sessionPermissions:  csync.NewMap[PermissionKey, bool](),
		autoApproveSessions: make(map[string]bool),
		allowedTools:        allowedTools,
		pendingRequests:     csync.NewMap[string, chan bool](),
	}
	svc.skip.Store(skip)
	return svc
}
