package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/backend"
	"github.com/rave-soft/braid/internal/db"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/session"
	"github.com/rave-soft/braid/internal/thread"
	"github.com/stretchr/testify/require"
)

// -- git test helpers, mirroring internal/thread/manager_test.go's style
// (that package's helpers are unexported to it, so this shells out
// directly rather than importing them). --

func requireGitForThreadsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForThreadsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func initThreadRepo(t *testing.T) string {
	t.Helper()
	requireGitForThreadsTest(t)

	dir := t.TempDir()
	runGitForThreadsTest(t, dir, "init", "-b", "main")
	runGitForThreadsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForThreadsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGitForThreadsTest(t, dir, "add", "-A")
	runGitForThreadsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

// -- fakes: a thread.Spawner whose spawned workspace never dispatches a
// real agent run, mirroring internal/thread/manager_test.go's fakeSpawner
// (unexported to that package, so reimplemented here against the exported
// thread.Spawner/thread.Handle interfaces). --

type fakeThreadSessions struct {
	session.Service
	mu sync.Mutex
	n  int
}

func (f *fakeThreadSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: uuid.New().String(), Title: title}, nil
}

// fakeThreadCoordinator records Run calls and returns immediately instead
// of dispatching a real agent turn, so Manager.Create's background
// dispatch goroutine cannot hang or panic against a workspace with no
// configured provider.
type fakeThreadCoordinator struct {
	agent.Coordinator
}

func (f *fakeThreadCoordinator) Run(_ context.Context, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeThreadCoordinator) CancelAll() {}

type fakeThreadHandle struct {
	id  string
	app *app.App
}

func (h *fakeThreadHandle) ID() string    { return h.id }
func (h *fakeThreadHandle) App() *app.App { return h.app }

type fakeThreadSpawner struct {
	t *testing.T
}

func (s *fakeThreadSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeThreadSessions{}
	a.AgentCoordinator = &fakeThreadCoordinator{}
	return &fakeThreadHandle{id: path, app: a}, nil
}

func (s *fakeThreadSpawner) Release(ctx context.Context, id string) error { return nil }

// newTestThreadStore builds a real, tempdir-backed thread.Store, mirroring
// internal/thread/manager_test.go's newTestStoreDB.
func newTestThreadStore(t *testing.T) thread.Store {
	t.Helper()
	dataDir := t.TempDir()
	// Deliberately does not call db.ResetPool: that closes every pooled
	// connection process-wide, which would tear down other thread tests'
	// connections when they run in parallel (each has its own tempdir,
	// hence its own pool entry — db.Release alone is sufficient here).
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return thread.NewStore(db.New(conn), dataDir)
}

// threadTestHarness wires a controllerV1 + httptest.Server around a
// synthetic workspace whose App has a real *thread.Manager attached
// (backed by a real git repo and a fake, LLM-free Spawner), mirroring how
// internal/backend/threads.go's attachServerThreads wires production
// workspaces.
type threadTestHarness struct {
	httpSrv *httptest.Server
	c       *controllerV1
	ws      *backend.Workspace
	mgr     *thread.Manager
}

func newThreadTestHarness(t *testing.T) *threadTestHarness {
	t.Helper()
	repo := initThreadRepo(t)

	appCtx, cancel := context.WithCancel(context.Background())
	a := app.NewForTest(appCtx)
	t.Cleanup(func() {
		cancel()
		a.ShutdownForTest()
	})

	mgr := thread.NewManager(thread.ManagerOptions{
		Store:       newTestThreadStore(t),
		Spawner:     &fakeThreadSpawner{t: t},
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetThreadManager(mgr)
	app.ForwardEvents(a, "thread", mgr.Subscribe)

	srv := &Server{}
	srv.backend = backend.New(context.Background(), nil, nil)
	srv.installHandler()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	ws := &backend.Workspace{
		ID:   uuid.New().String(),
		Path: repo,
		App:  a,
	}
	backend.SetWorkspaceShutdownFnForTest(ws, func() {})
	backend.InsertWorkspaceForTest(srv.backend, ws)

	return &threadTestHarness{
		httpSrv: hs,
		c:       &controllerV1{backend: srv.backend, server: srv},
		ws:      ws,
		mgr:     mgr,
	}
}

func (h *threadTestHarness) url(path string) string {
	return h.httpSrv.URL + "/v1/workspaces/" + h.ws.ID + "/threads" + path
}

func (h *threadTestHarness) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.url(path), reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpSrv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

// TestHandleWorkspaceThreads_NoManager verifies that a workspace without a
// thread manager (e.g. a non-git workspace, or one where attachServerThreads
// / attachLocalThreads never ran) reports 409 rather than 404: the
// workspace itself is fine, it just doesn't support threads.
func TestHandleWorkspaceThreads_NoManager(t *testing.T) {
	t.Parallel()
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)

	srv := &Server{backend: c.backend}
	srv.installHandler()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+"/v1/workspaces/"+ws.ID+"/threads", nil)
	require.NoError(t, err)
	resp, err := hs.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestHandleWorkspaceThread_NotFound verifies that GET/send/delete on an
// unknown thread ID return 404 against a workspace that does support
// threads.
func TestHandleWorkspaceThread_NotFound(t *testing.T) {
	t.Parallel()
	h := newThreadTestHarness(t)

	resp := h.do(t, http.MethodGet, "/does-not-exist", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = h.do(t, http.MethodPost, "/does-not-exist/send", proto.SendThreadRequest{Message: "hi"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = h.do(t, http.MethodDelete, "/does-not-exist", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleWorkspaceThreads_CRUD drives the full create -> list -> get ->
// send -> delete lifecycle over HTTP against a real *thread.Manager (real
// git worktree/branch, fake LLM-free Spawner).
func TestHandleWorkspaceThreads_CRUD(t *testing.T) {
	t.Parallel()
	h := newThreadTestHarness(t)

	// Create.
	resp := h.do(t, http.MethodPost, "", proto.CreateThreadRequest{
		Name: "feature-x",
		Goal: "do the thing",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var created proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "feature-x", created.Name)
	require.Equal(t, "running", created.Status)

	// List.
	resp = h.do(t, http.MethodGet, "", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list []proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list, 1)
	require.Equal(t, created.ID, list[0].ID)

	// Get by ID.
	resp = h.do(t, http.MethodGet, "/"+created.ID, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, created.ID, got.ID)

	// Send a follow-up message.
	resp = h.do(t, http.MethodPost, "/"+created.ID+"/send", proto.SendThreadRequest{Message: "keep going"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Delete (force=true: the thread is still "running" from Create/Send).
	resp = h.do(t, http.MethodDelete, "/"+created.ID+"?force=true", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = h.do(t, http.MethodGet, "/"+created.ID, nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleWorkspaceThreads_CreateInvalid verifies that a validation
// failure from Manager.Create (invalid name) surfaces as 400, not 500.
func TestHandleWorkspaceThreads_CreateInvalid(t *testing.T) {
	t.Parallel()
	h := newThreadTestHarness(t)

	resp := h.do(t, http.MethodPost, "", proto.CreateThreadRequest{
		Name: "Not A Valid Name!",
		Goal: "do the thing",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestThreadEvent_DeliveredOverSSE verifies that a thread lifecycle event
// published by the manager (via ForwardEvents, wired the same way
// attachServerThreads wires production workspaces) reaches the
// workspace's SSE stream wrapped as pubsub.PayloadTypeThreadEvent with a
// decodable proto.ThreadEvent payload.
func TestThreadEvent_DeliveredOverSSE(t *testing.T) {
	t.Parallel()
	h := newThreadTestHarness(t)

	events, err := h.c.backend.SubscribeEvents(t.Context(), h.ws.ID)
	require.NoError(t, err)

	resp := h.do(t, http.MethodPost, "", proto.CreateThreadRequest{
		Name: "sse-check",
		Goal: "do the thing",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var created proto.Thread
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	for {
		select {
		case ev := <-events:
			wrapped := wrapEvent(ev.Payload)
			if wrapped == nil || wrapped.Type != pubsub.PayloadTypeThreadEvent {
				continue
			}
			var decoded pubsub.Event[proto.ThreadEvent]
			require.NoError(t, json.Unmarshal(wrapped.Payload, &decoded))
			if decoded.Payload.Thread.ID != created.ID {
				continue
			}
			require.Equal(t, proto.ThreadEventCreated, decoded.Payload.Type)
			return
		case <-t.Context().Done():
			t.Fatal("timed out waiting for the thread created event over SSE")
		}
	}
}
