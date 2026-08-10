package backend

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/proto"
	"github.com/rave-soft/braid/internal/strand"
)

// strandHandle is the [strand.Handle] returned by [strandSpawner]. id is
// the backend workspace ID (not the internal client ID used to hold it
// open); Release looks the client ID back up from it.
type strandHandle struct {
	id  string
	app *app.App
}

func (h *strandHandle) ID() string    { return h.id }
func (h *strandHandle) App() *app.App { return h.app }

// strandSpawner adapts [Backend] to [strand.Spawner]: it drives strand
// workspaces through the same CreateWorkspace bootstrap path every other
// workspace uses, but registers an internal hold rather than requiring an
// SSE client to attach. Internal holds behave exactly like a live SSE
// stream to the refcount/idle-shutdown machinery in backend.go (see
// AttachClient/DetachClient): they keep the workspace alive with no grace
// timer racing the strand's run, however long it takes, and release
// immediately (no detach grace) when the strand is done with it.
type strandSpawner struct {
	backend *Backend

	mu       sync.Mutex
	clientOf map[string]string // workspace ID -> internal client ID holding it
}

// StrandSpawner returns a [strand.Spawner] backed by this Backend.
func (b *Backend) StrandSpawner() strand.Spawner {
	return &strandSpawner{backend: b, clientOf: make(map[string]string)}
}

// Spawn implements strand.Spawner.
func (s *strandSpawner) Spawn(ctx context.Context, path string) (strand.Handle, error) {
	clientID := uuid.New().String()
	// attachStrands=false: a strand's own workspace must not get a strand
	// manager of its own — nesting is not supported.
	ws, _, err := s.backend.createWorkspace(proto.Workspace{Path: path, ClientID: clientID}, false)
	if err != nil {
		return nil, err
	}
	// AttachClient converts the creation hold (which expires after
	// createGrace) into a stream claim (which does not expire on its
	// own), exactly as a client's first SSE connection would.
	if err := s.backend.AttachClient(ws.ID, clientID); err != nil {
		_ = s.backend.releaseHold(ws.ID, clientID)
		return nil, fmt.Errorf("strand: hold workspace open: %w", err)
	}

	s.mu.Lock()
	s.clientOf[ws.ID] = clientID
	s.mu.Unlock()

	return &strandHandle{id: ws.ID, app: ws.App}, nil
}

// Release implements strand.Spawner.
func (s *strandSpawner) Release(ctx context.Context, id string) error {
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
