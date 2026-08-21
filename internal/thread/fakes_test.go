// Package thread_test holds this package's own tests. It is split from
// package thread (the in-package tests in store_test.go, which need no
// fakes at all) because everything below spawns real *app.App values to
// exercise thread.Manager/TaskManager end to end, and internal/app is the
// composition root that imports this package — a package-thread test file
// importing internal/app back would close that cycle the moment
// production code (internal/app.go) names *thread.Manager, which is
// exactly the point of keeping the two packages properly layered. See
// githelpers_test.go for the git-repo scaffolding shared across this
// package's files.
package thread_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakes and manager scaffolding for this package's own tests, plus
// test_adapters_test.go's exported adapters. Where production code would
// use a threadspawn type, a small local stand-in is used instead, since
// threadspawn cannot be imported here (threadspawn imports thread, and
// this package's tests already import thread — importing threadspawn too
// would make thread.Manager reachable through two different adapter
// implementations depending on path, which is not worth chasing down for
// tests, so threadspawn stays untouched by this package):
//   - testAppWorkspace adapts *app.App into the domain [thread.Workspace]
//     using the exported test adapters in test_adapters_test.go.
//   - thread.NewStoreForTest (store_testing.go) builds a sqlc-backed
//     Store inline, because threadspawn.NewStore cannot be imported here.
// ---------------------------------------------------------------------------

// testAppWorkspace adapts an *app.App to the domain [Workspace] for tests
// that hand a raw App to ManagerOptions.ParentApp. It is the in-package
// stand-in for threadspawn.AppWorkspaceAdapter (unimportable here without a
// test import cycle) and uses the same exported domain-view adapters the
// composition seam uses in production. The coordinator adapter is cached so
// every Coordinator() call returns the same value: the manager registers a
// delegation's parent link with the adapter it got from the parent workspace,
// and the tests then compare that recorded link against the parent's own
// coordinator by identity, which only holds if the adapter is stable.
type testAppWorkspace struct {
	app *app.App

	mu sync.Mutex
	co *testCoordinatorAdapter
	rc *testRunCompletionBrokerAdapter
}

var _ thread.Workspace = (*testAppWorkspace)(nil)

func (w *testAppWorkspace) Coordinator() thread.Coordinator {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.co == nil {
		// Mirror the production adapter's nil handling (see
		// threadspawn.NewCoordinatorAdapter): an App that never initialized
		// a coordinator presents a nil Coordinator, not a non-nil adapter
		// wrapping nil.
		if c := w.app.Coordinator(); c != nil {
			w.co = &testCoordinatorAdapter{inner: c}
		}
	}
	return w.co
}

func (w *testAppWorkspace) Sessions() thread.SessionService {
	if w.app.Sessions() == nil {
		return nil
	}
	return NewTestSessionService(w.app.Sessions())
}

func (w *testAppWorkspace) Messages() thread.MessageService {
	if w.app.Messages() == nil {
		return nil
	}
	return NewTestMessageService(w.app.Messages())
}

func (w *testAppWorkspace) Permissions() permission.Service {
	return w.app.Permissions()
}

func (w *testAppWorkspace) RunCompletions() thread.RunCompletionBroker {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rc == nil {
		w.rc = &testRunCompletionBrokerAdapter{inner: w.app.RunCompletions()}
	}
	return w.rc
}

func (w *testAppWorkspace) SendEvent(msg any) {
	w.app.SendEvent(msg)
}

// sendErr drops a Send's [SendDisposition], for the many tests that only
// assert the message was accepted. Tests that care what the disposition
// says read it directly instead.
func sendErr(_ thread.SendDisposition, err error) error { return err }

// testCoordinatorAdapter wraps an agent.Coordinator into the domain's
// narrow Coordinator port for in-package tests. It mirrors the production
// adapter in threadspawn but lives here because the test files are in
// package thread and cannot import threadspawn.
type testCoordinatorAdapter struct {
	inner agent.Coordinator
}

var _ thread.Coordinator = (*testCoordinatorAdapter)(nil)

// testTranslateCtx mirrors the production coordinatorAdapter.translateCtx
// (see internal/app/threadspawn/coordinator_adapter.go): it re-applies the
// domain's per-run RunID and agent-dispatch origin tag onto the agent's own
// context keys, so the fake records the same RunID/origin the real
// coordinator would observe. Without this the domain's WithAgentDispatch /
// WithRunID tags (carried on the domain's own keys) would be dropped and a
// dispatched run's origin would read empty.
func testTranslateCtx(ctx context.Context) context.Context {
	if runID := thread.RunIDFromContext(ctx); runID != "" {
		ctx = agent.WithRunID(ctx, runID)
	}
	if thread.AgentDispatchFromContext(ctx) {
		ctx = agent.WithAgentDispatch(ctx)
	}
	if onDispatch, ok := thread.SteeringFromContext(ctx); ok {
		ctx = agent.WithSteering(ctx, func(outcome agent.SteerOutcome) {
			if onDispatch == nil {
				return
			}
			switch outcome {
			case agent.SteerEnqueued:
				onDispatch(thread.DispatchFolded)
			case agent.SteerCanceled:
				onDispatch(thread.DispatchCancelled)
			default:
				onDispatch(thread.DispatchRan)
			}
		})
	}
	return ctx
}

func (a *testCoordinatorAdapter) RunAccepted(ctx context.Context, accept any, sessionID, prompt string, attachments []thread.Attachment) error {
	ar, _ := accept.(*agent.AcceptedRun)
	msgAttachments := make([]message.Attachment, 0, len(attachments))
	for _, at := range attachments {
		msgAttachments = append(msgAttachments, message.Attachment{
			FilePath: at.FilePath,
			FileName: at.FileName,
			MimeType: at.MimeType,
			Content:  at.Content,
		})
	}
	_, err := a.inner.RunAccepted(testTranslateCtx(ctx), ar, sessionID, prompt, msgAttachments...)
	return err
}

func (a *testCoordinatorAdapter) BeginAccepted(sessionID string) any {
	return a.inner.BeginAccepted(sessionID)
}

func (a *testCoordinatorAdapter) Cancel(sessionID string) {
	a.inner.Cancel(sessionID)
}

func (a *testCoordinatorAdapter) SessionQueue(sessionID string) (bool, int) {
	return a.inner.IsSessionBusy(sessionID), a.inner.QueuedPrompts(sessionID)
}

func (a *testCoordinatorAdapter) RegisterDelegationParent(sessionID string, parent thread.DelegationParent) {
	a.inner.RegisterDelegationParent(sessionID, agent.DelegationParent{
		Parent:          testUnwrapCoordinator(parent.Parent),
		ParentSessionID: parent.ParentSessionID,
		DelegationID:    parent.DelegationID,
		Kind:            parent.Kind,
		Name:            parent.Name,
		Depth:           parent.Depth,
	})
}

// testUnwrapCoordinator recovers the concrete agent.Coordinator a domain
// [Coordinator] wraps, for the fake to record. Every Coordinator the domain
// sees in these tests is a *testCoordinatorAdapter produced by
// testAppWorkspace.Coordinator, so the assertion is safe; a nil or foreign
// value degrades to nil, matching the domain's "no parent" handling.
func testUnwrapCoordinator(c thread.Coordinator) agent.Coordinator {
	if a, ok := c.(*testCoordinatorAdapter); ok {
		return a.inner
	}
	return nil
}

func (a *testCoordinatorAdapter) DeliverTaskCompletion(ctx context.Context, parentSessionID string, completion thread.TaskCompletion) {
	a.inner.DeliverTaskCompletion(ctx, parentSessionID, agent.TaskCompletion{
		DelegationID:   completion.DelegationID,
		Kind:           completion.Kind,
		Name:           completion.Name,
		Goal:           completion.Goal,
		Status:         completion.Status,
		ChildSessionID: completion.ChildSessionID,
		ResultText:     completion.ResultText,
		Error:          completion.Error,
		Depth:          completion.Depth,
		TerminalAt:     completion.TerminalAt,
	})
}

// testRunCompletionBrokerAdapter wraps *pubsub.Broker[notify.RunComplete]
// into the domain's narrow RunCompletionBroker for in-package tests.
type testRunCompletionBrokerAdapter struct {
	inner *pubsub.Broker[notify.RunComplete]
}

var _ thread.RunCompletionBroker = (*testRunCompletionBrokerAdapter)(nil)

func (a *testRunCompletionBrokerAdapter) Subscribe(ctx context.Context) <-chan pubsub.Event[thread.RunComplete] {
	in := a.inner.Subscribe(ctx)
	out := make(chan pubsub.Event[thread.RunComplete])
	go func() {
		defer close(out)
		for ev := range in {
			out <- pubsub.Event[thread.RunComplete]{
				Type: ev.Type,
				Payload: thread.RunComplete{
					SessionID: ev.Payload.SessionID,
					RunID:     ev.Payload.RunID,
					MessageID: ev.Payload.MessageID,
					Text:      ev.Payload.Text,
					Error:     ev.Payload.Error,
					Cancelled: ev.Payload.Cancelled,
				},
			}
		}
	}()
	return out
}

func (a *testRunCompletionBrokerAdapter) Publish(typ pubsub.EventType, v thread.RunComplete) {
	a.inner.Publish(typ, notify.RunComplete{
		SessionID: v.SessionID,
		RunID:     v.RunID,
		MessageID: v.MessageID,
		Text:      v.Text,
		Error:     v.Error,
		Cancelled: v.Cancelled,
	})
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeSessions implements the session creation methods used by the manager.
type fakeSessions struct {
	session.Service
	mu             sync.Mutex
	n              int
	createdSession session.Session
	// createErr, when set, makes both Create and CreateTaskSession fail
	// instead of fabricating a session — for tests driving Manager.Create's
	// rollback on a session-creation failure.
	createErr error
}

func (f *fakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return session.Session{}, f.createErr
	}
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

func (f *fakeSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return session.Session{}, f.createErr
	}
	f.createdSession = session.Session{ID: id, ParentSessionID: parentSessionID, Title: title}
	return f.createdSession, nil
}

// Get returns the one session CreateTaskSession fabricated, if id matches
// it. deliverTaskCompletion (lifecycle.go) calls this to resolve a task's
// parent session.
func (f *fakeSessions) Get(_ context.Context, id string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createdSession.ID == id {
		return f.createdSession, nil
	}
	return session.Session{}, fmt.Errorf("thread: fakeSessions: session %q not found", id)
}

// fakeCoordinator implements agent.Coordinator, recording Run calls and
// Cancel invocations. It does not publish RunComplete itself — tests drive
// that explicitly through the owning app's RunCompletions broker.
type fakeCoordinator struct {
	agent.Coordinator

	mu                sync.Mutex
	runs              []fakeRun
	cancelAllCalled   bool
	canceled          []string
	runErr            error
	delivered         []deliveredCompletion
	registeredParents []registeredParent
	// busy/queued back IsSessionBusy/QueuedPrompts so a test can put the
	// session in the state lifecycle.send reports as a queued delivery.
	busy   bool
	queued int
	// cancelOnEntry makes a steering dispatch resolve the way the real
	// coordinator resolves one that a cancel already covered when it
	// arrived: neither run nor folded. Only reachable because the dispatch
	// reserves acceptance — see agent.SteerCanceled.
	cancelOnEntry bool
}

// setCancelOnEntry makes every subsequent steering dispatch land on the
// cancelled branch.
func (f *fakeCoordinator) setCancelOnEntry(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelOnEntry = v
}

// setQueue makes the fake report sessions as mid-turn with queued prompts
// waiting, the shape [lifecycle.send] turns into a queued SendDisposition.
func (f *fakeCoordinator) setQueue(busy bool, queued int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = busy
	f.queued = queued
}

// registeredParent pairs a RegisterDelegationParent call's child session
// id with the DelegationParent it recorded.
type registeredParent struct {
	sessionID string
	parent    thread.DelegationParent
}

func (f *fakeCoordinator) RegisterDelegationParent(sessionID string, parent agent.DelegationParent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registeredParents = append(f.registeredParents, registeredParent{sessionID: sessionID, parent: thread.DelegationParent{
		Parent:          &testCoordinatorAdapter{inner: parent.Parent},
		ParentSessionID: parent.ParentSessionID,
		DelegationID:    parent.DelegationID,
		Kind:            parent.Kind,
		Name:            parent.Name,
		Depth:           parent.Depth,
	}})
}

func (f *fakeCoordinator) registeredDelegationParents() []registeredParent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registeredParent(nil), f.registeredParents...)
}

// deliveredCompletion pairs a DeliverTaskCompletion call's target session
// with the event it carried.
type deliveredCompletion struct {
	sessionID  string
	completion thread.TaskCompletion
}

func (f *fakeCoordinator) DeliverTaskCompletion(_ context.Context, sessionID string, completion agent.TaskCompletion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, deliveredCompletion{sessionID: sessionID, completion: thread.TaskCompletion{
		DelegationID:   completion.DelegationID,
		Kind:           completion.Kind,
		Name:           completion.Name,
		Goal:           completion.Goal,
		Status:         completion.Status,
		ChildSessionID: completion.ChildSessionID,
		ResultText:     completion.ResultText,
		Error:          completion.Error,
		Depth:          completion.Depth,
		TerminalAt:     completion.TerminalAt,
	}})
}

func (f *fakeCoordinator) deliveredCompletions() []deliveredCompletion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deliveredCompletion(nil), f.delivered...)
}

type fakeRun struct {
	sessionID  string
	prompt     string
	runID      string
	delegation permission.DelegationRef
	origin     message.Origin
}

// dispatch records a dispatch and, for a steering call, reproduces the
// real decision sessionAgent.run would have taken under its per-session
// mutex: a busy session folds the call into the turn in flight (dropping
// its RunID on the way into the queue, exactly as the real one does), an
// idle session runs it as its own turn under that RunID. The hook fires
// after the fake's own lock is released, matching production's "the hook
// must not take a lock this call already holds".
func (f *fakeCoordinator) dispatch(ctx context.Context, sessionID, prompt string) error {
	onDispatch, steering := agent.SteeringFromContext(ctx)
	f.mu.Lock()
	if steering && f.cancelOnEntry {
		f.mu.Unlock()
		if onDispatch != nil {
			onDispatch(agent.SteerCanceled)
		}
		return nil
	}
	runID := agent.RunIDFromContext(ctx)
	folded := steering && f.busy
	if folded {
		runID = ""
	}
	f.runs = append(f.runs, fakeRun{
		sessionID:  sessionID,
		prompt:     prompt,
		runID:      runID,
		delegation: permission.DelegationFromContext(ctx),
		origin:     agent.PromptOriginFromContext(ctx),
	})
	err := f.runErr
	f.mu.Unlock()
	if steering && onDispatch != nil {
		if folded {
			onDispatch(agent.SteerEnqueued)
		} else {
			onDispatch(agent.SteerRan)
		}
	}
	return err
}

func (f *fakeCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, f.dispatch(ctx, sessionID, prompt)
}

func (f *fakeCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *fakeCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, f.dispatch(ctx, sessionID, prompt)
}

func (f *fakeCoordinator) CancelAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelAllCalled = true
}

func (f *fakeCoordinator) Cancel(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = append(f.canceled, sessionID)
}

func (f *fakeCoordinator) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func (f *fakeCoordinator) canceledSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.canceled...)
}

func (f *fakeCoordinator) cancelAllWasCalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancelAllCalled
}

// SetThreads / SetTasks / IsBusy are no-ops: a fakeCoordinator assigned as
// an App's AgentCoordinator goes through the real App.SetThreads/App.Shutdown
// paths, which call both — the embedded nil agent.Coordinator would panic.
func (f *fakeCoordinator) SetThreads(tools.ThreadManager) {}
func (f *fakeCoordinator) SetTasks(tools.TaskManager)     {}
func (f *fakeCoordinator) IsBusy() bool                   { return false }

// The remaining Coordinator methods are not exercised by the thread tests;
// they exist only so the fake satisfies the interface without the embedded
// nil coordinator panicking if one is ever reached.
func (f *fakeCoordinator) Steer(context.Context, agent.SessionAgentCall) (agent.SteerOutcome, *fantasy.AgentResult, error) {
	return 0, nil, f.runErr
}

func (f *fakeCoordinator) IsSessionBusy(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func (f *fakeCoordinator) QueuedPrompts(string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queued
}

func (f *fakeCoordinator) QueuedPromptsList(string) []string                  { return nil }
func (f *fakeCoordinator) ClearQueue(string)                                  {}
func (f *fakeCoordinator) Summarize(context.Context, string) error            { return nil }
func (f *fakeCoordinator) Model() agent.Model                                 { return agent.Model{} }
func (f *fakeCoordinator) UpdateModels(context.Context) error                 { return nil }
func (f *fakeCoordinator) GenerateTitle(context.Context, string, string)      {}
func (f *fakeCoordinator) RefreshSkills([]*skills.Skill, []*skills.Skill)     {}
func (f *fakeCoordinator) SendToParent(context.Context, string, string) error { return nil }

// fakeHandle is the [Handle] returned by fakeSpawner.
type fakeHandle struct {
	id  string
	app *app.App
}

func (h *fakeHandle) ID() string    { return h.id }
func (h *fakeHandle) App() *app.App { return h.app }
func (h *fakeHandle) Workspace() thread.Workspace {
	return &testAppWorkspace{app: h.app}
}

// fakeSpawner spawns a real (but network/db-free) app.App per call via
// app.NewForTest, wired with fakeSessions and a fakeCoordinator instead of
// the real ones a full bootstrap would build. It keeps every spawned app
// reachable by worktree path so tests can drive a thread's run to
// completion by publishing to that app's RunCompletions broker directly.
type fakeSpawner struct {
	t *testing.T

	mu           sync.Mutex
	byPath       map[string]*fakeHandle
	coordByPath  map[string]*fakeCoordinator
	released     map[string]bool
	releaseCount map[string]int
	// releaseSawWorktree records, per id, whether the worktree at that
	// path was still on disk at the moment Release was called — see
	// Release and releaseSawWorktreeAt.
	releaseSawWorktree map[string]bool
	spawnCount         int
	spawnErr           error
	runErr             error
	blockSpawn         bool
	spawnEntered       chan struct{}
	spawnRelease       chan struct{}
	// sessionsErr, when set, is handed to every fakeSessions this spawner
	// builds, so its Create/CreateTaskSession calls fail — for tests
	// driving Manager.Create's rollback on a session-creation failure.
	sessionsErr error
	// afterSpawn, when set, runs after a successful Spawn has built its
	// handle but before returning it — for tests that need to cancel the
	// manager's own context exactly between a successful spawn and the
	// caller's next ctx.Err() check (Manager.Create/Activate), which
	// nothing else can time deterministically.
	afterSpawn func(path string)
}

func newFakeSpawner(t *testing.T) *fakeSpawner {
	return &fakeSpawner{
		t:            t,
		byPath:       make(map[string]*fakeHandle),
		coordByPath:  make(map[string]*fakeCoordinator),
		released:     make(map[string]bool),
		releaseCount: make(map[string]int),
	}
}

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	if s.blockSpawn {
		close(s.spawnEntered)
		<-ctx.Done()
		<-s.spawnRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spawnCount++
	if s.spawnErr != nil {
		return nil, s.spawnErr
	}

	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.SetSessionsForTest(&fakeSessions{createErr: s.sessionsErr})
	coord := &fakeCoordinator{runErr: s.runErr}
	a.AgentCoordinator = coord

	h := &fakeHandle{id: path, app: a}
	s.byPath[path] = h
	s.coordByPath[path] = coord
	if s.afterSpawn != nil {
		s.afterSpawn(path)
	}
	return h, nil
}

func (s *fakeSpawner) spawns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnCount
}

func (s *fakeSpawner) Release(ctx context.Context, id string) error {
	// Recorded before taking the lock, and before mutating any of this
	// fake's own state: id is the worktree path itself (see Spawn), so a
	// caller unwinding release-before-worktree-removal (the correct
	// order — see [unwinder]'s doc comment) always finds it still present
	// here. TestManager_CreateRollbackOrder_ReleasesBeforeRemovingWorktree
	// is what actually asserts on this.
	_, statErr := os.Stat(id)
	sawWorktree := statErr == nil

	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[id] = true
	s.releaseCount[id]++
	if s.releaseSawWorktree == nil {
		s.releaseSawWorktree = make(map[string]bool)
	}
	s.releaseSawWorktree[id] = sawWorktree
	return nil
}

// releaseSawWorktreeAt reports whether the worktree at id was still present
// on disk at the moment Release was called for it.
func (s *fakeSpawner) releaseSawWorktreeAt(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseSawWorktree[id]
}

func (s *fakeSpawner) releases(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.releaseCount[id]
}

func (s *fakeSpawner) appFor(path string) *app.App {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPath[path].app
}

func (s *fakeSpawner) handleFor(path string) *fakeHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPath[path]
}

func (s *fakeSpawner) coordFor(path string) *fakeCoordinator {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coordByPath[path]
}

func (s *fakeSpawner) wasReleased(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released[id]
}

// settleTimeout bounds a Manager.Wait in these tests.
//
// Generous on purpose: Wait returns the moment the threads settle, so the
// number only decides how long a genuinely stuck test hangs before failing —
// while too small a number fails tests that were merely slow. It was two
// seconds, and CI duly failed with "context deadline exceeded" on a runner
// executing the whole suite at once.
const settleTimeout = 60 * time.Second

// newTestManager wires a Manager over a real store, a real git repo (repo),
// and the fakeSpawner defined above.
func newTestManager(t *testing.T, repo string) (*thread.Manager, *fakeSpawner) {
	t.Helper()
	spawner := newFakeSpawner(t)
	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       thread.NewStoreForTest(t),
		Spawner:     spawner,
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	return mgr, spawner
}

// publishSuccess simulates a thread's agent run finishing successfully.
func publishSuccess(t *testing.T, a *app.App, sessionID string) {
	t.Helper()
	coord := a.AgentCoordinator.(*fakeCoordinator)
	require.Eventually(t, func() bool { return coord.runCount() > 0 }, time.Second, time.Millisecond)
	coord.mu.Lock()
	runID := coord.runs[len(coord.runs)-1].runID
	coord.mu.Unlock()
	a.RunCompletions().Publish(pubsub.UpdatedEvent, notify.RunComplete{SessionID: sessionID, RunID: runID, Text: "finished"})
}
