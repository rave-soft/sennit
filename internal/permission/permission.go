package permission

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/csync"
	"github.com/rave-soft/braid/internal/pubsub"
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
	Delegation DelegationRef `json:"delegation,omitempty"`
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
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
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	AutoApproveSession(sessionID string)
	SetSkipRequests(skip bool)
	// ActiveRequest returns the request currently awaiting an answer,
	// for subscribers that need to recover one they did not see
	// published. See the implementation for why this is needed at all.
	ActiveRequest() (PermissionRequest, bool)
	SkipRequests() bool
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	// ConfineToWorkingDir marks this workspace as one that may not write
	// outside its working directory at all. See ConfinedDir.
	ConfineToWorkingDir()
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

// PermissionKey is a composite key for session permission lookups.
type PermissionKey struct {
	SessionID string
	ToolName  string
	Action    string
	Path      string
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
		}, true)
	})
}

func (s *permissionService) Grant(permission PermissionRequest) bool {
	return s.resolve(permission, true, false, nil)
}

func (s *permissionService) Deny(permission PermissionRequest) bool {
	return s.resolve(permission, false, true, nil)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	// tell the UI that a permission was requested
	s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}
	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
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
	}); ok {
		s.notificationBroker.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, PermissionNotification{
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
			s.dispatchNext(permission.ID)
		}
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
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

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
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
