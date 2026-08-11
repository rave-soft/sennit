package backend

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/thread"
)

// threadHandle is the [thread.Handle] returned by [threadSpawner]. id is
// the backend workspace ID (not the internal client ID used to hold it
// open); Release looks the client ID back up from it.
type threadHandle struct {
	id  string
	app *app.App
}

func (h *threadHandle) ID() string    { return h.id }
func (h *threadHandle) App() *app.App { return h.app }

// threadSpawner adapts [Backend] to [thread.Spawner]: it drives thread
// workspaces through the same CreateWorkspace bootstrap path every other
// workspace uses, but registers an internal hold rather than requiring an
// SSE client to attach. Internal holds behave exactly like a live SSE
// stream to the refcount/idle-shutdown machinery in backend.go (see
// AttachClient/DetachClient): they keep the workspace alive with no grace
// timer racing the thread's run, however long it takes, and release
// immediately (no detach grace) when the thread is done with it.
type threadSpawner struct {
	backend      *Backend
	parentAgents func() map[string]config.Agent

	mu       sync.Mutex
	clientOf map[string]string // workspace ID -> internal client ID holding it
}

// ThreadSpawner returns a [thread.Spawner] backed by this Backend.
func (b *Backend) ThreadSpawner(parentAgents func() map[string]config.Agent) thread.Spawner {
	return &threadSpawner{
		backend:      b,
		parentAgents: parentAgents,
		clientOf:     make(map[string]string),
	}
}

// Spawn implements thread.Spawner.
func (s *threadSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	clientID := uuid.New().String()
	var inheritedAgents map[string]config.Agent
	if s.parentAgents != nil {
		inheritedAgents = s.parentAgents()
	}
	// attachThreads=false: a thread's own workspace must not get a thread
	// manager of its own — nesting is not supported.
	ws, _, err := s.backend.createWorkspace(proto.Workspace{Path: path, ClientID: clientID}, false, inheritedAgents)
	if err != nil {
		return nil, err
	}
	// AttachClient converts the creation hold (which expires after
	// createGrace) into a stream claim (which does not expire on its
	// own), exactly as a client's first SSE connection would.
	if err := s.backend.AttachClient(ws.ID, clientID); err != nil {
		_ = s.backend.releaseHold(ws.ID, clientID)
		return nil, fmt.Errorf("thread: hold workspace open: %w", err)
	}

	s.mu.Lock()
	s.clientOf[ws.ID] = clientID
	s.mu.Unlock()

	return &threadHandle{id: ws.ID, app: ws.App}, nil
}

// Release implements thread.Spawner.
func (s *threadSpawner) Release(ctx context.Context, id string) error {
	s.mu.Lock()
	clientID, ok := s.clientOf[id]
	delete(s.clientOf, id)
	s.mu.Unlock()
	if !ok {
		return nil
	}

	// Mark the hold as explicitly released before dropping the stream so
	// the final DetachClient tears the workspace down immediately
	// instead of waiting out the reconnect grace for a client that is
	// not coming back.
	if err := s.backend.releaseHold(id, clientID); err != nil {
		return err
	}
	s.backend.DetachClient(id, clientID)
	return nil
}
