package threadspawn

import (
	"context"

	"github.com/google/uuid"
	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/herdr"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/rave-soft/sennit/internal/workspace"
)

// localHandle is the [thread.Handle] returned by [LocalSpawner].
type localHandle struct {
	id        string
	app       *app.App
	workspace *AppWorkspaceAdapter
}

func (h *localHandle) ID() string                  { return h.id }
func (h *localHandle) Workspace() thread.Workspace { return h.workspace }

// LocalSpawner spawns thread workspaces by bootstrapping a plain
// in-process app.App directly, for the single-process CLI. Each spawned
// app joins the repository workspace lock (WorkspaceLock), running
// concurrently alongside the top-level workspace's app in the same
// process.
type LocalSpawner struct {
	apps         *csync.Map[string, *app.App]
	parentAgents func() map[string]config.Agent
	parentSkills func() []*skills.Skill
	parentYOLO   func() bool
	parentModel  func() config.SelectedModel
	frontend     func(*app.App) workspace.Workspace
}

// NewLocalSpawner returns a ready-to-use LocalSpawner. parentSkills, when
// non-nil, supplies the parent workspace's skills to every thread it
// spawns; see skills.DiscoveryConfig.InheritedSkills for why a thread
// cannot find them on its own. parentModel, when non-nil, is read at every
// Spawn so a thread runs the model its parent is running right now -- see
// app.BootstrapOptions.PreferredModel.
func NewLocalSpawner(
	parentAgents func() map[string]config.Agent,
	parentSkills func() []*skills.Skill,
	parentYOLO func() bool,
	parentModel func() config.SelectedModel,
	frontend ...func(*app.App) workspace.Workspace,
) *LocalSpawner {
	var frontendFactory func(*app.App) workspace.Workspace
	if len(frontend) != 0 {
		frontendFactory = frontend[0]
	}
	return &LocalSpawner{
		apps:         csync.NewMap[string, *app.App](),
		parentAgents: parentAgents,
		parentSkills: parentSkills,
		parentYOLO:   parentYOLO,
		parentModel:  parentModel,
		frontend:     frontendFactory,
	}
}

// Apps returns the apps of every thread this spawner currently holds, for
// callers that must reach live threads after they started — the parent's
// skill watcher pushes a re-discovered skill set into each.
func (s *LocalSpawner) Apps() []*app.App {
	out := make([]*app.App, 0, s.apps.Len())
	for _, a := range s.apps.Seq2() {
		out = append(out, a)
	}
	return out
}

// bootstrapOptions builds the app.BootstrapOptions every thread spawn
// uses. Split out from Spawn so tests can inspect those options without
// driving a full Bootstrap. It reads only the parent's state, not the
// spawn path, which Bootstrap takes separately.
func (s *LocalSpawner) bootstrapOptions() app.BootstrapOptions {
	var inheritedAgents map[string]config.Agent
	if s.parentAgents != nil {
		inheritedAgents = s.parentAgents()
	}
	var inheritedSkills []*skills.Skill
	if s.parentSkills != nil {
		inheritedSkills = s.parentSkills()
	}
	var yolo bool
	if s.parentYOLO != nil {
		yolo = s.parentYOLO()
	}
	// Read per Spawn, not once at construction: a thread that is stopped
	// and started again is a fresh Spawn, and it has to pick up whatever
	// the parent is running by then.
	var model *config.SelectedModel
	if s.parentModel != nil {
		if m := s.parentModel(); m.Model != "" {
			model = &m
		}
	}
	return app.BootstrapOptions{
		WorkspaceLock:   true,
		InheritedAgents: inheritedAgents,
		InheritedSkills: inheritedSkills,
		PreferredModel:  model,
		YOLO:            yolo,
		ConfineWrites:   true,
		// A thread reports into its own workspace, not the user's pane:
		// the pane belongs to the top-level session, and falling back to
		// the process-wide herdr client here would make the thread's App
		// share it with the parent's -- Release shutting the thread down
		// would then send pane.release_agent and free the pane out from
		// under a parent session that is still running.
		HerdrClient: func() *herdr.Client { return nil },
	}
}

// Spawn implements thread.Spawner.
func (s *LocalSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	boot, err := app.Bootstrap(ctx, path, s.bootstrapOptions())
	if err != nil {
		return nil, err
	}
	var frontendWorkspace workspace.Workspace
	if s.frontend != nil {
		frontendWorkspace = s.frontend(boot.App)
	}
	id := uuid.New().String()
	h := &localHandle{
		id:        id,
		app:       boot.App,
		workspace: NewAppWorkspaceAdapter(boot.App, frontendWorkspace),
	}
	s.apps.Set(id, boot.App)
	return h, nil
}

// Release implements thread.Spawner.
func (s *LocalSpawner) Release(ctx context.Context, id string) error {
	a, ok := s.apps.Take(id)
	if !ok {
		return nil
	}
	a.Shutdown()
	return nil
}

// parentHandle is the [thread.Handle] [ParentAppSpawner] returns: it wraps
// the caller's own App instead of building one.
type parentHandle struct {
	id        string
	workspace thread.Workspace
}

func (h *parentHandle) ID() string                  { return h.id }
func (h *parentHandle) Workspace() thread.Workspace { return h.workspace }

// ParentAppSpawner is the thread.Spawner for tasks: unlike LocalSpawner, it
// does not bootstrap a second isolated App per delegation. A task shares its
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
	workspace thread.Workspace
}

// NewParentAppSpawner returns a thread.Spawner whose every Spawn call
// returns a Handle wrapping the caller's own already-running workspace. The
// caller owns workspace and must use the same instance for ManagerOptions.ParentApp.
func NewParentAppSpawner(workspace thread.Workspace) *ParentAppSpawner {
	return &ParentAppSpawner{workspace: workspace}
}

// Spawn implements thread.Spawner.
func (s *ParentAppSpawner) Spawn(ctx context.Context, path string) (thread.Handle, error) {
	return &parentHandle{id: uuid.New().String(), workspace: s.workspace}, nil
}

// Release implements thread.Spawner. See [ParentAppSpawner]'s doc comment
// for why this must stay a no-op.
func (s *ParentAppSpawner) Release(ctx context.Context, id string) error {
	return nil
}
