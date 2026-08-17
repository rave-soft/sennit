package thread

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakes and manager scaffolding for the thread package's own tests.
//
// These live in package thread (not thread_test) because the internal test
// files reference them by short name. They must not import
// internal/app/threadspawn (that would be a test import cycle: threadspawn
// imports thread). Where the original code used a threadspawn type, a small
// local stand-in is used instead:
//   - testAppWorkspace adapts *app.App into the domain [Workspace] using the
//     exported test adapters in test_adapters_test.go.
//   - newTestStoreDB builds a sqlc-backed Store inline (the same queries
//     threadspawn.NewStore uses), because that constructor cannot be imported
//     here.
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

var _ Workspace = (*testAppWorkspace)(nil)

func (w *testAppWorkspace) Coordinator() Coordinator {
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

func (w *testAppWorkspace) Sessions() SessionService {
	if w.app.Sessions() == nil {
		return nil
	}
	return NewTestSessionService(w.app.Sessions())
}

func (w *testAppWorkspace) Messages() MessageService {
	if w.app.Messages() == nil {
		return nil
	}
	return NewTestMessageService(w.app.Messages())
}

func (w *testAppWorkspace) Permissions() permission.Service {
	return w.app.Permissions()
}

func (w *testAppWorkspace) RunCompletions() RunCompletionBroker {
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
func sendErr(_ SendDisposition, err error) error { return err }

// testCoordinatorAdapter wraps an agent.Coordinator into the domain's
// narrow Coordinator port for in-package tests. It mirrors the production
// adapter in threadspawn but lives here because the test files are in
// package thread and cannot import threadspawn.
type testCoordinatorAdapter struct {
	inner agent.Coordinator
}

var _ Coordinator = (*testCoordinatorAdapter)(nil)

// testTranslateCtx mirrors the production coordinatorAdapter.translateCtx
// (see internal/app/threadspawn/coordinator_adapter.go): it re-applies the
// domain's per-run RunID and agent-dispatch origin tag onto the agent's own
// context keys, so the fake records the same RunID/origin the real
// coordinator would observe. Without this the domain's WithAgentDispatch /
// WithRunID tags (carried on the domain's own keys) would be dropped and a
// dispatched run's origin would read empty.
func testTranslateCtx(ctx context.Context) context.Context {
	if runID := RunIDFromContext(ctx); runID != "" {
		ctx = agent.WithRunID(ctx, runID)
	}
	if AgentDispatchFromContext(ctx) {
		ctx = agent.WithAgentDispatch(ctx)
	}
	return ctx
}

func (a *testCoordinatorAdapter) Run(ctx context.Context, sessionID, prompt string) error {
	_, err := a.inner.Run(testTranslateCtx(ctx), sessionID, prompt)
	return err
}

func (a *testCoordinatorAdapter) RunAccepted(ctx context.Context, accept any, sessionID, prompt string) error {
	ar, _ := accept.(*agent.AcceptedRun)
	_, err := a.inner.RunAccepted(testTranslateCtx(ctx), ar, sessionID, prompt)
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

func (a *testCoordinatorAdapter) RegisterDelegationParent(sessionID string, parent DelegationParent) {
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
func testUnwrapCoordinator(c Coordinator) agent.Coordinator {
	if a, ok := c.(*testCoordinatorAdapter); ok {
		return a.inner
	}
	return nil
}

func (a *testCoordinatorAdapter) DeliverTaskCompletion(ctx context.Context, parentSessionID string, completion TaskCompletion) {
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

var _ RunCompletionBroker = (*testRunCompletionBrokerAdapter)(nil)

func (a *testRunCompletionBrokerAdapter) Subscribe(ctx context.Context) <-chan pubsub.Event[RunComplete] {
	in := a.inner.Subscribe(ctx)
	out := make(chan pubsub.Event[RunComplete])
	go func() {
		defer close(out)
		for ev := range in {
			out <- pubsub.Event[RunComplete]{
				Type: ev.Type,
				Payload: RunComplete{
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

func (a *testRunCompletionBrokerAdapter) Publish(typ pubsub.EventType, v RunComplete) {
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
// store scaffolding
// ---------------------------------------------------------------------------

// newTestStoreDB builds a real (sqlite-backed) Store using the same sqlc
// queries threadspawn.NewStore uses, scoped to a throwaway project path. It
// lives here because the internal test files call it and this package cannot
// import threadspawn.
func newTestStoreDB(t *testing.T) Store {
	t.Helper()
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return &testStoreDB{q: db.New(conn), projectPath: dataDir}
}

// testStoreDB is a minimal Store over the sqlc queries, mirroring the
// threadspawn implementation (which cannot be imported here).
type testStoreDB struct {
	q           db.Querier
	projectPath string
}

var _ Store = (*testStoreDB)(nil)

func (s *testStoreDB) Create(ctx context.Context, params CreateParams) (Thread, error) {
	kind := params.Kind
	if kind == "" {
		kind = KindThread
	}
	mergePolicy := params.MergePolicy
	if mergePolicy == "" && kind == KindThread {
		mergePolicy = MergeAuto
	}
	dbThread, err := s.q.CreateThread(ctx, db.CreateThreadParams{
		ID:              fmt.Sprintf("thread-%d", time.Now().UnixNano()),
		Name:            params.Name,
		ProjectPath:     s.projectPath,
		Goal:            params.Goal,
		BaseBranch:      params.BaseBranch,
		Branch:          params.Branch,
		WorktreePath:    params.WorktreePath,
		SessionID:       params.SessionID,
		Status:          string(StatusPending),
		MergePolicy:     string(mergePolicy),
		Kind:            string(kind),
		ParentSessionID: params.ParentSessionID,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) Get(ctx context.Context, id string) (Thread, error) {
	dbThread, err := s.q.GetThread(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) GetByName(ctx context.Context, name string) (Thread, error) {
	dbThread, err := s.q.GetThreadByName(ctx, db.GetThreadByNameParams{
		Name:        name,
		ProjectPath: s.projectPath,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) List(ctx context.Context) ([]Thread, error) {
	rows, err := s.q.ListThreads(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, len(rows))
	for i, r := range rows {
		out[i] = testFromDBItem(r)
	}
	return out, nil
}

func (s *testStoreDB) ListAll(ctx context.Context) ([]Thread, error) {
	rows, err := s.q.ListThreadsAll(ctx, s.projectPath)
	if err != nil {
		return nil, err
	}
	out := make([]Thread, len(rows))
	for i, r := range rows {
		out[i] = testFromDBItem(r)
	}
	return out, nil
}

func (s *testStoreDB) SetStatus(ctx context.Context, id string, params SetStatusParams) (Thread, error) {
	dbThread, err := s.q.UpdateThreadStatus(ctx, db.UpdateThreadStatusParams{
		ID:            id,
		Status:        string(params.Status),
		Error:         params.Error,
		ResultSummary: params.ResultSummary,
		CompletedAt:   sqlInt64(params.CompletedAt),
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) SetSession(ctx context.Context, id, sessionID string) (Thread, error) {
	dbThread, err := s.q.UpdateThreadSession(ctx, db.UpdateThreadSessionParams{
		ID:        id,
		SessionID: sessionID,
	})
	if err != nil {
		return Thread{}, err
	}
	return testFromDBItem(dbThread), nil
}

func (s *testStoreDB) Delete(ctx context.Context, id string) error {
	return s.q.DeleteThread(ctx, id)
}

// sqlInt64 mirrors the threadspawn store's CompletedAt handling: zero leaves
// the column NULL.
func sqlInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v != 0}
}

// testFromDBItem mirrors threadspawn.fromDBItem (which cannot be imported
// here); the field mapping is identical.
func testFromDBItem(item db.Thread) Thread {
	return Thread{
		Delegation: Delegation{
			ID:              item.ID,
			Name:            item.Name,
			Goal:            item.Goal,
			SessionID:       item.SessionID,
			Status:          Status(item.Status),
			Kind:            Kind(item.Kind),
			ResultSummary:   item.ResultSummary,
			Error:           item.Error,
			CreatedAt:       item.CreatedAt,
			UpdatedAt:       item.UpdatedAt,
			CompletedAt:     item.CompletedAt.Int64,
			ParentSessionID: item.ParentSessionID,
		},
		BaseBranch:   item.BaseBranch,
		Branch:       item.Branch,
		WorktreePath: item.WorktreePath,
		MergePolicy:  MergePolicy(item.MergePolicy),
	}
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
}

func (f *fakeSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: fmt.Sprintf("sess-%d", f.n), Title: title}, nil
}

func (f *fakeSessions) CreateTaskSession(_ context.Context, id, parentSessionID, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	parent    DelegationParent
}

func (f *fakeCoordinator) RegisterDelegationParent(sessionID string, parent agent.DelegationParent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registeredParents = append(f.registeredParents, registeredParent{sessionID: sessionID, parent: DelegationParent{
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
	completion TaskCompletion
}

func (f *fakeCoordinator) DeliverTaskCompletion(_ context.Context, sessionID string, completion agent.TaskCompletion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, deliveredCompletion{sessionID: sessionID, completion: TaskCompletion{
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

func (f *fakeCoordinator) Run(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, fakeRun{
		sessionID:  sessionID,
		prompt:     prompt,
		runID:      agent.RunIDFromContext(ctx),
		delegation: permission.DelegationFromContext(ctx),
		origin:     agent.PromptOriginFromContext(ctx),
	})
	return nil, f.runErr
}

func (f *fakeCoordinator) BeginAccepted(string) *agent.AcceptedRun { return nil }

func (f *fakeCoordinator) RunAccepted(ctx context.Context, _ *agent.AcceptedRun, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, fakeRun{
		sessionID:  sessionID,
		prompt:     prompt,
		runID:      agent.RunIDFromContext(ctx),
		delegation: permission.DelegationFromContext(ctx),
		origin:     agent.PromptOriginFromContext(ctx),
	})
	err := f.runErr
	f.mu.Unlock()
	return nil, err
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
func (h *fakeHandle) Workspace() Workspace {
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
	spawnCount   int
	spawnErr     error
	runErr       error
	blockSpawn   bool
	spawnEntered chan struct{}
	spawnRelease chan struct{}
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

func (s *fakeSpawner) Spawn(ctx context.Context, path string) (Handle, error) {
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
	a.SetSessionsForTest(&fakeSessions{})
	coord := &fakeCoordinator{runErr: s.runErr}
	a.AgentCoordinator = coord

	h := &fakeHandle{id: path, app: a}
	s.byPath[path] = h
	s.coordByPath[path] = coord
	return h, nil
}

func (s *fakeSpawner) spawns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawnCount
}

func (s *fakeSpawner) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released[id] = true
	s.releaseCount[id]++
	return nil
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
func newTestManager(t *testing.T, repo string) (*Manager, *fakeSpawner) {
	t.Helper()
	spawner := newFakeSpawner(t)
	mgr := NewManager(ManagerOptions{
		Store:       newTestStoreDB(t),
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
