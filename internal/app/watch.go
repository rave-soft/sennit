package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/skills"
)

// WorkspaceChanged is published on app.events whenever this App's config
// or skill catalog was reloaded because an external edit was detected by
// the watchers started in New (config.ConfigStore.WatchForExternalChanges
// / skills.WatchForChanges) — for example an agent's Edit/Write tool
// adding an MCP server to .sennit/sennit.json directly, bypassing
// SetConfigFields.
//
// It carries no payload; subscribers that cache a derived snapshot should
// refetch theirs. The UI does not need to handle this type specially: it
// already reacts to the underlying MCP/skills events these same reloads
// publish via app.events (see setupEvents).
type WorkspaceChanged struct{}

// startExternalChangeWatchers wires the config and skills external-change
// pollers to this App and starts them, bound to app's own lifetime rather
// than a caller's. Previously these were started only by the now-removed
// client/server backend, so local mode — the only mode — never picked up
// an externally-edited config or skill file without a restart.
//
// A dedicated context/WaitGroup pair, not app.eventsCtx/serviceEventsWG:
// those belong to setupEvents' own fan-in. The shutdown hook cancels and
// joins the pollers before MCP closes, so a watcher cannot start a new MCP
// reinitialization against a closing registry.
func (app *App) startExternalChangeWatchers(ctx context.Context) {
	watchCtx, cancel := context.WithCancel(ctx)
	var watcherWG sync.WaitGroup
	var reinitializeWG sync.WaitGroup

	app.config.OnExternalChange(func() {
		// Re-init MCP servers whose config changed, against this app's own
		// registry. Run async so the poll loop is never blocked on MCP
		// reconciliation.
		reinitializeWG.Go(func() { app.MCP.Reinitialize(watchCtx, app.config) })
		app.applyConfigPermissionsBypass()
		app.publishWorkspaceChanged()
	})
	watcherWG.Go(func() { app.config.WatchForExternalChanges(watchCtx) })

	if app.Skills != nil {
		watcherWG.Go(func() {
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

	if err := app.AddPreCleanupHook(func(context.Context) error {
		cancel()
		watcherWG.Wait()
		reinitializeWG.Wait()
		return nil
	}); err != nil {
		// Shutdown has already started. Do not leave the pollers running when
		// their owner cannot register a join hook.
		cancel()
		watcherWG.Wait()
		reinitializeWG.Wait()
		slog.Error("Failed to register external watcher pre-cleanup", "error", err)
	}
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
	app.SetPermissionsSkip(bypass)
	app.lastConfigBypass = bypass
}

// publishWorkspaceChanged publishes a WorkspaceChanged marker on app.events.
func (app *App) publishWorkspaceChanged() {
	app.SendEvent(pubsub.Event[WorkspaceChanged]{
		Type:    pubsub.UpdatedEvent,
		Payload: WorkspaceChanged{},
	})
}
