package thread

import (
	"context"

	"github.com/google/uuid"
	"github.com/rave-soft/braid/internal/app"
	"github.com/rave-soft/braid/internal/config"
	"github.com/rave-soft/braid/internal/csync"
)

// Handle is a running workspace hosting one thread's isolated app.App:
// its own .braid data directory, database, and agent coordinator, rooted
// at the thread's git worktree.
type Handle interface {
	// ID identifies the handle to its owning [Spawner]; opaque to the
	// manager, which only ever passes it back to [Spawner.Release].
	ID() string
	// App is the thread's isolated application instance.
	App() *app.App
}

// Spawner bootstraps and tears down the isolated workspace backing a
// thread. Implementations differ in how the resulting workspace's
// lifecycle interacts with the rest of the process — see [LocalSpawner]
// for the in-process CLI case and internal/backend/thread_spawner.go for
// the client/server case.
type Spawner interface {
	// Spawn bootstraps an app.App rooted at path (a thread's git
	// worktree) and returns a handle to it.
	Spawn(ctx context.Context, path string) (Handle, error)
	// Release tears the workspace identified by id (a value previously
	// returned by Handle.ID) down. Idempotent: releasing an unknown or
	// already-released id is a no-op.
	Release(ctx context.Context, id string) error
}

// localHandle is the [Handle] implementation returned by [LocalSpawner].
type localHandle struct {
	id  string
	app *app.App
}

func (h *localHandle) ID() string    { return h.id }
func (h *localHandle) App() *app.App { return h.app }

// LocalSpawner spawns thread workspaces by bootstrapping a plain
// in-process app.App directly, for the single-process CLI. Each spawned
// app owns its own workspace lock (WorkspaceLock) but does not mirror
// its skills into the process-wide globals (GlobalSkillsMirror is off),
// matching how the backend hosts multiple concurrent workspaces.
type LocalSpawner struct {
	apps         *csync.Map[string, *app.App]
	parentAgents func() map[string]config.Agent
	parentYOLO   func() bool
}

// NewLocalSpawner returns a ready-to-use LocalSpawner.
func NewLocalSpawner(parentAgents func() map[string]config.Agent, parentYOLO func() bool) *LocalSpawner {
	return &LocalSpawner{
		apps:         csync.NewMap[string, *app.App](),
		parentAgents: parentAgents,
		parentYOLO:   parentYOLO,
	}
}

// Spawn implements Spawner.
func (s *LocalSpawner) Spawn(ctx context.Context, path string) (Handle, error) {
	var inheritedAgents map[string]config.Agent
	if s.parentAgents != nil {
		inheritedAgents = s.parentAgents()
	}
	var yolo bool
	if s.parentYOLO != nil {
		yolo = s.parentYOLO()
	}
	boot, err := app.Bootstrap(ctx, path, app.BootstrapOptions{
		WorkspaceLock:      true,
		GlobalSkillsMirror: false,
		InheritedAgents:    inheritedAgents,
		YOLO:               yolo,
	})
	if err != nil {
		return nil, err
	}
	id := uuid.New().String()
	s.apps.Set(id, boot.App)
	return &localHandle{id: id, app: boot.App}, nil
}

// Release implements Spawner.
func (s *LocalSpawner) Release(ctx context.Context, id string) error {
	a, ok := s.apps.Take(id)
	if !ok {
		return nil
	}
	a.Shutdown()
	return nil
}
