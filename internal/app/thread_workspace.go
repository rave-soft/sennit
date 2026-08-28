package app

import (
	"github.com/rave-soft/sennit/internal/agent"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	sessionstore "github.com/rave-soft/sennit/internal/session/store"
)

// These accessors exist so an *App satisfies thread.Workspace — the
// narrow, consumer-side interface internal/thread declares for the slice
// of a workspace App its delegation lifecycle drives. Go forbids a method
// and a field sharing a name, which is why the three services that used
// to be exported App fields are now unexported with these accessors on
// top; the accessors return the same values the fields used to expose
// directly, so production behavior is unchanged.
//
// The dependency direction is app → thread, never the reverse:
// internal/thread must not import internal/app (the composition root that
// imports thread's types), so *App satisfies this interface structurally
// and is handed in as a thread.Workspace by whichever spawner produced it
// (see internal/app/threadspawn).
//
// app does import internal/thread — services.go names *thread.Manager and
// *thread.TaskManager for the two concretely-typed accessors — and
// internal/thread's own tests import app back for realistic *app.App
// fakes (see internal/thread/fakes_test.go). That is legal because those
// fakes live in package thread_test, whose imports are not part of
// internal/thread's own import graph; a production import from thread to
// app is what would cycle. An earlier version of this comment claimed app
// kept no thread import at all, and the Threads/Tasks seams were said to
// exist for that reason — they exist because the agent-tool layer needs a
// narrower interface than the manager, which is a different argument.

// Coordinator returns this workspace's agent coordinator, or nil if it
// has not been initialized yet (an unconfigured project). Safe to call
// concurrently with setCoordinator: initCoderAgent and
// SetDelegationManagers can swap the coordinator from one goroutine while
// AppWorkspace's request-handling methods read it from others.
func (app *App) Coordinator() agent.Coordinator {
	app.agentCoordinatorMu.RLock()
	defer app.agentCoordinatorMu.RUnlock()
	return app.agentCoordinator
}

// setCoordinator installs coordinator as this workspace's agent
// coordinator. See Coordinator's doc for why this needs a lock.
func (app *App) setCoordinator(coordinator agent.Coordinator) {
	app.agentCoordinatorMu.Lock()
	defer app.agentCoordinatorMu.Unlock()
	app.agentCoordinator = coordinator
}

// Sessions returns this workspace's session service.
func (app *App) Sessions() sessionstore.Service { return app.sessions }

// Messages returns this workspace's message service.
func (app *App) Messages() messagestore.Service { return app.messages }

// Permissions returns this workspace's permission service.
func (app *App) Permissions() permission.Service { return app.permissions }
