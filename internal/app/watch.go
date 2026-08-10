package app

import (
	"context"
	"sync"

	"github.com/rave-soft/braid/internal/pubsub"
	"github.com/rave-soft/braid/internal/skills"
)

// WorkspaceChanged is published on app.events whenever this App's config
// or skill catalog was reloaded because an external edit was detected by
// the watchers started in New (config.ConfigStore.WatchForExternalChanges
// / skills.WatchForChanges) — for example an agent's Edit/Write tool
// adding an MCP server to .braid/braid.json directly, bypassing
// SetConfigFields.
//
// It carries no payload; subscribers that cache a derived snapshot
// (backend's workspaceToProto, for remote/client-server clients) should
// refetch theirs — see internal/backend/backend.go, which subscribes to
// app.Events and translates this into a workspace-scoped
// proto.ConfigChanged. Local-mode UI does not need to handle this type
// specially: it already reacts to the underlying MCP/skills events these
// same reloads publish via app.events (see setupEvents).
type WorkspaceChanged struct{}

// startExternalChangeWatchers wires the config and skills external-change
// pollers to this App and starts them, bound to app's own lifetime rather
// than a caller's. Both local mode (a bare App, no backend) and
// client/server mode (backend.Workspace, which embeds App) get hot-reload
// from this single place. Previously these were started only in
// backend.createWorkspace, so local mode — the default — never picked up
// an externally-edited config or skill file without a restart.
//
// A dedicated context/WaitGroup pair, not app.eventsCtx/serviceEventsWG:
// those belong to setupEvents' own fan-in and are torn down by its own
// cleanupFunc. Keeping this pair separate means its cleanupFunc can
// cancel-then-wait without reasoning about ordering against the event
// fan-in's shutdown.
func (app *App) startExternalChangeWatchers(ctx context.Context) {
	watchCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup

	app.config.OnExternalChange(func() {
		// Re-init MCP servers whose config changed, against this app's own
		// registry. Run async so the poll loop is never blocked on MCP
		// reconciliation (mirrors backend's former publishConfigChanged).
		go app.MCP.Reinitialize(watchCtx, app.config)
		app.applyConfigPermissionsBypass()
		app.publishWorkspaceChanged()
	})
	wg.Go(func() { app.config.WatchForExternalChanges(watchCtx) })

	if app.Skills != nil {
		wg.Go(func() {
			skills.WatchForChanges(watchCtx, SkillsDiscoveryConfig(app.config), app.Skills, 0, func() {
				// Unlike config, skill discovery is not re-run by anything
				// else, so the watcher's onChange must also refresh the
				// coordinator's cached skill snapshot — ReplaceDiscovery
				// alone only updates app.Skills, which buildTools does not
				// read from directly.
				if app.AgentCoordinator != nil {
					app.AgentCoordinator.RefreshSkills(app.Skills.AllSkills(), app.Skills.ActiveSkills())
				}
				app.publishWorkspaceChanged()
			})
		})
	}

	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		cancel()
		wg.Wait()
		return nil
	})
}

// applyConfigPermissionsBypass re-applies the persisted permissions.bypass
// value to the live permission service when a config reload changed it.
// It compares against app.lastConfigBypass — the config value as of the
// last time it was applied — rather than Permissions.SkipRequests(), so a
// session-only ctrl+y / /yolo toggle (which also lives on Permissions) is
// left alone when a reload fires for an unrelated reason (e.g. a provider
// edit) and permissions.bypass itself did not change.
func (app *App) applyConfigPermissionsBypass() {
	var bypass bool
	if cfg := app.config.Config(); cfg.Permissions != nil {
		bypass = cfg.Permissions.Bypass
	}
	if bypass == app.lastConfigBypass {
		return
	}
	app.Permissions.SetSkipRequests(bypass)
	app.lastConfigBypass = bypass
}

// publishWorkspaceChanged publishes a WorkspaceChanged marker on app.events.
func (app *App) publishWorkspaceChanged() {
	app.SendEvent(pubsub.Event[WorkspaceChanged]{
		Type:    pubsub.UpdatedEvent,
		Payload: WorkspaceChanged{},
	})
}
