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
// app joins the repository workspace lock (WorkspaceLock) but does not mirror
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
		// A thread exists to keep its work on its own branch, in its own
		// worktree. Inheriting yolo from the main agent removes the
		// prompt that would otherwise stand between it and the rest of
		// the disk, so the boundary has to be enforced rather than asked
		// about.
		ConfineWrites: true,
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

// parentHandle is the [Handle] [ParentAppSpawner] returns: it wraps the
// caller's own App instead of building one.
type parentHandle struct {
	id  string
	app *app.App
}

func (h *parentHandle) ID() string    { return h.id }
func (h *parentHandle) App() *app.App { return h.app }

// ParentAppSpawner is the Spawner for tasks: unlike LocalSpawner, it does
// not bootstrap a second isolated App per delegation. A task shares its
// parent workspace's own working directory, so bootstrapping a second App
// over it would take the same WorkspaceLock and open the same SQLite file
// the parent already holds — redundant at best, and it would throw away
// the point of a task being cheap to start. Spawn returns a Handle
// wrapping the given App directly (ignoring path — a task has no
// worktree of its own).
//
// Release is deliberately a no-op: the parent App outlives every task run
// inside it and is torn down by its own owner, never by this Spawner.
// Getting this backwards — having Release call App.Shutdown — would tear
// the user's own workspace down out from under them the moment a task's
// run ends or the process shuts down.
type ParentAppSpawner struct {
	app *app.App
}

// NewParentAppSpawner returns a Spawner whose every Spawn call returns a
// Handle wrapping a, the caller's own already-running App.
func NewParentAppSpawner(a *app.App) *ParentAppSpawner {
	return &ParentAppSpawner{app: a}
}

// Spawn implements Spawner.
func (s *ParentAppSpawner) Spawn(ctx context.Context, path string) (Handle, error) {
	return &parentHandle{id: uuid.New().String(), app: s.app}, nil
}

// Release implements Spawner. See [ParentAppSpawner]'s doc comment for why
// this must stay a no-op.
func (s *ParentAppSpawner) Release(ctx context.Context, id string) error {
	return nil
}
