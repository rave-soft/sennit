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
	"github.com/rave-soft/braid/internal/strand"
	"github.com/stretchr/testify/require"
)

// -- git test helpers, mirroring internal/strand/manager_test.go's style
// (that package's helpers are unexported to it, so this shells out
// directly rather than importing them). --

func requireGitForStrandsTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found on PATH")
	}
}

func runGitForStrandsTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func initStrandRepo(t *testing.T) string {
	t.Helper()
	requireGitForStrandsTest(t)

	dir := t.TempDir()
	runGitForStrandsTest(t, dir, "init", "-b", "main")
	runGitForStrandsTest(t, dir, "config", "user.email", "test@example.com")
	runGitForStrandsTest(t, dir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644))
	runGitForStrandsTest(t, dir, "add", "-A")
	runGitForStrandsTest(t, dir, "commit", "-m", "initial commit")
	return dir
}

// -- fakes: a strand.Spawner whose spawned workspace never dispatches a
// real agent run, mirroring internal/strand/manager_test.go's fakeSpawner
// (unexported to that package, so reimplemented here against the exported
// strand.Spawner/strand.Handle interfaces). --

type fakeStrandSessions struct {
	session.Service
	mu sync.Mutex
	n  int
}

func (f *fakeStrandSessions) Create(_ context.Context, title string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return session.Session{ID: uuid.New().String(), Title: title}, nil
}

// fakeStrandCoordinator records Run calls and returns immediately instead
// of dispatching a real agent turn, so Manager.Create's background
// dispatch goroutine cannot hang or panic against a workspace with no
// configured provider.
type fakeStrandCoordinator struct {
	agent.Coordinator
}

func (f *fakeStrandCoordinator) Run(_ context.Context, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (f *fakeStrandCoordinator) CancelAll() {}

type fakeStrandHandle struct {
	id  string
	app *app.App
}

func (h *fakeStrandHandle) ID() string    { return h.id }
func (h *fakeStrandHandle) App() *app.App { return h.app }

type fakeStrandSpawner struct {
	t *testing.T
}

func (s *fakeStrandSpawner) Spawn(ctx context.Context, path string) (strand.Handle, error) {
	a := app.NewForTest(context.Background())
	s.t.Cleanup(a.ShutdownForTest)
	a.Sessions = &fakeStrandSessions{}
	a.AgentCoordinator = &fakeStrandCoordinator{}
	return &fakeStrandHandle{id: path, app: a}, nil
}

func (s *fakeStrandSpawner) Release(ctx context.Context, id string) error { return nil }

// newTestStrandStore builds a real, tempdir-backed strand.Store, mirroring
// internal/strand/manager_test.go's newTestStoreDB.
func newTestStrandStore(t *testing.T) strand.Store {
	t.Helper()
	dataDir := t.TempDir()
	// Deliberately does not call db.ResetPool: that closes every pooled
	// connection process-wide, which would tear down other strand tests'
	// connections when they run in parallel (each has its own tempdir,
	// hence its own pool entry — db.Release alone is sufficient here).
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	return strand.NewStore(db.New(conn))
}

// strandTestHarness wires a controllerV1 + httptest.Server around a
// synthetic workspace whose App has a real *strand.Manager attached
// (backed by a real git repo and a fake, LLM-free Spawner), mirroring how
// internal/backend/strands.go's attachServerStrands wires production
// workspaces.
type strandTestHarness struct {
	httpSrv *httptest.Server
	c       *controllerV1
	ws      *backend.Workspace
	mgr     *strand.Manager
}

func newStrandTestHarness(t *testing.T) *strandTestHarness {
	t.Helper()
	repo := initStrandRepo(t)

	appCtx, cancel := context.WithCancel(context.Background())
	a := app.NewForTest(appCtx)
	t.Cleanup(func() {
		cancel()
		a.ShutdownForTest()
	})

	mgr := strand.NewManager(strand.ManagerOptions{
		Store:       newTestStrandStore(t),
		Spawner:     &fakeStrandSpawner{t: t},
		RepoRoot:    repo,
		WorktreeDir: t.TempDir(),
	})
	a.SetStrandManager(mgr)
	app.ForwardEvents(a, "strand", mgr.Subscribe)

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

	return &strandTestHarness{
		httpSrv: hs,
		c:       &controllerV1{backend: srv.backend, server: srv},
		ws:      ws,
		mgr:     mgr,
	}
}

func (h *strandTestHarness) url(path string) string {
	return h.httpSrv.URL + "/v1/workspaces/" + h.ws.ID + "/strands" + path
}

func (h *strandTestHarness) do(t *testing.T, method, path string, body any) *http.Response {
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
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestHandleWorkspaceStrands_NoManager verifies that a workspace without a
// strand manager (e.g. a non-git workspace, or one where attachServerStrands
// / attachLocalStrands never ran) reports 409 rather than 404: the
// workspace itself is fine, it just doesn't support strands.
func TestHandleWorkspaceStrands_NoManager(t *testing.T) {
	t.Parallel()
	c := newTestController()
	ws := installSyntheticWorkspace(t, c)

	srv := &Server{backend: c.backend}
	srv.installHandler()
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp, err := hs.Client().Get(hs.URL + "/v1/workspaces/" + ws.ID + "/strands")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

// TestHandleWorkspaceStrand_NotFound verifies that GET/send/delete on an
// unknown strand ID return 404 against a workspace that does support
// strands.
func TestHandleWorkspaceStrand_NotFound(t *testing.T) {
	t.Parallel()
	h := newStrandTestHarness(t)

	resp := h.do(t, http.MethodGet, "/does-not-exist", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = h.do(t, http.MethodPost, "/does-not-exist/send", proto.SendStrandRequest{Message: "hi"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp = h.do(t, http.MethodDelete, "/does-not-exist", nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleWorkspaceStrands_CRUD drives the full create -> list -> get ->
// send -> delete lifecycle over HTTP against a real *strand.Manager (real
// git worktree/branch, fake LLM-free Spawner).
func TestHandleWorkspaceStrands_CRUD(t *testing.T) {
	t.Parallel()
	h := newStrandTestHarness(t)

	// Create.
	resp := h.do(t, http.MethodPost, "", proto.CreateStrandRequest{
		Name: "feature-x",
		Goal: "do the thing",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var created proto.Strand
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.NotEmpty(t, created.ID)
	require.Equal(t, "feature-x", created.Name)
	require.Equal(t, "running", created.Status)

	// List.
	resp = h.do(t, http.MethodGet, "", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list []proto.Strand
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list, 1)
	require.Equal(t, created.ID, list[0].ID)

	// Get by ID.
	resp = h.do(t, http.MethodGet, "/"+created.ID, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got proto.Strand
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, created.ID, got.ID)

	// Send a follow-up message.
	resp = h.do(t, http.MethodPost, "/"+created.ID+"/send", proto.SendStrandRequest{Message: "keep going"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Delete (force=true: the strand is still "running" from Create/Send).
	resp = h.do(t, http.MethodDelete, "/"+created.ID+"?force=true", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = h.do(t, http.MethodGet, "/"+created.ID, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestHandleWorkspaceStrands_CreateInvalid verifies that a validation
// failure from Manager.Create (invalid name) surfaces as 400, not 500.
func TestHandleWorkspaceStrands_CreateInvalid(t *testing.T) {
	t.Parallel()
	h := newStrandTestHarness(t)

	resp := h.do(t, http.MethodPost, "", proto.CreateStrandRequest{
		Name: "Not A Valid Name!",
		Goal: "do the thing",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestStrandEvent_DeliveredOverSSE verifies that a strand lifecycle event
// published by the manager (via ForwardEvents, wired the same way
// attachServerStrands wires production workspaces) reaches the
// workspace's SSE stream wrapped as pubsub.PayloadTypeStrandEvent with a
// decodable proto.StrandEvent payload.
func TestStrandEvent_DeliveredOverSSE(t *testing.T) {
	t.Parallel()
	h := newStrandTestHarness(t)

	events, err := h.c.backend.SubscribeEvents(t.Context(), h.ws.ID)
	require.NoError(t, err)

	resp := h.do(t, http.MethodPost, "", proto.CreateStrandRequest{
		Name: "sse-check",
		Goal: "do the thing",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var created proto.Strand
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	for {
		select {
		case ev := <-events:
			wrapped := wrapEvent(ev.Payload)
			if wrapped == nil || wrapped.Type != pubsub.PayloadTypeStrandEvent {
				continue
			}
			var decoded pubsub.Event[proto.StrandEvent]
			require.NoError(t, json.Unmarshal(wrapped.Payload, &decoded))
			if decoded.Payload.Strand.ID != created.ID {
				continue
			}
			require.Equal(t, proto.StrandEventCreated, decoded.Payload.Type)
			return
		case <-t.Context().Done():
			t.Fatal("timed out waiting for the strand created event over SSE")
		}
	}
}
