package app

import (
	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/message"
	"github.com/rave-soft/braid/internal/permission"
	"github.com/rave-soft/braid/internal/session"
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
// (see internal/app/threadspawn). app deliberately keeps no import of
// internal/thread: the structural satisfaction is the whole point, and
// the thread managers this App carries (see the Threads/Tasks fields)
// stay typed through the agent-tool seams and the untyped accessors for
// the same reason.

// Coordinator returns this workspace's agent coordinator, or nil if it
// has not been initialized yet (an unconfigured project).
func (app *App) Coordinator() agent.Coordinator { return app.AgentCoordinator }

// Sessions returns this workspace's session service.
func (app *App) Sessions() session.Service { return app.sessions }

// Messages returns this workspace's message service.
func (app *App) Messages() message.Service { return app.messages }

// Permissions returns this workspace's permission service.
func (app *App) Permissions() permission.Service { return app.permissions }
