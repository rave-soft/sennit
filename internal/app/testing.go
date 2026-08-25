package app

import (
	"context"
	"sync"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/config/credentials"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/pubsub"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/shell"
)

// Test-only accessors for the services that used to be exported App fields
// (now unexported, exposed to the thread domain through the accessors in
// thread_workspace.go). Production code keeps using those accessors; these
// exist purely so tests can still install or read the services directly.
func (app *App) SetSessionsForTest(s session.Service)       { app.sessions = s }
func (app *App) SessionsForTest() session.Service           { return app.sessions }
func (app *App) SetMessagesForTest(m messagestore.Service)  { app.messages = m }
func (app *App) MessagesForTest() messagestore.Service      { return app.messages }
func (app *App) SetPermissionsForTest(p permission.Service) { app.permissions = p }
func (app *App) PermissionsForTest() permission.Service     { return app.permissions }

// SetAgentCoordinatorForTest installs coordinator directly, bypassing
// InitCoderAgent. AgentCoordinator itself stays a plain assignable field
// (promoted from appServices), but a bare &App{} literal from outside this
// package can no longer set it inline the way it could before appServices
// existed, since appServices is unexported: construct the App with New or
// NewForTest first (so appServices is non-nil), then call this.
func (app *App) SetAgentCoordinatorForTest(coordinator agent.Coordinator) {
	app.AgentCoordinator = coordinator
}

// SetConfigForTest installs store as this App's config store and rebuilds
// its credentials manager to match, mirroring how New wires the two
// together. It exists for tests (outside this package) that need a real
// App.Credentials() without booting a full New (database, LSP, MCP,
// agent coordinator).
func (app *App) SetConfigForTest(store *config.ConfigStore) {
	app.config = store
	app.credentials = credentials.New(store)
}

// NewForTest constructs a minimal [App] suitable for in-process tests
// that need a working event broker and permission service without
// booting a real config, database, LSP, MCP, or agent coordinator.
//
// The returned App has:
//
//   - A live `events` broker that [App.SendEvent] publishes to and
//     [App.Events] subscribes from.
//   - A real [permission.Service] whose request and notification
//     brokers are fanned into the events broker, so subscribers to
//     [App.Events] observe the same permission events the production
//     wiring would deliver to SSE clients.
//   - An [App.agentNotifications] broker.
//   - A non-nil [App.AgentDispatcher], the same as a production App, so
//     tests can dispatch fire-and-forget runs against a fake
//     agent.Coordinator assigned to AgentCoordinator and exercise
//     [App.Shutdown]'s join ordering without booting a real one.
//
// The caller owns lifetime: cancel ctx (or call [App.Shutdown]) to
// tear down the fan-in goroutines and the events broker.
func NewForTest(ctx context.Context) *App {
	app := &App{
		appServices: appServices{
			permissions:      permission.NewPermissionService("", false, nil),
			Questions:        question.NewService(),
			BackgroundShells: shell.NewBackgroundShellManager(),
		},
		appEvents: appEvents{
			events:             pubsub.NewBroker[any](),
			serviceEventsWG:    &sync.WaitGroup{},
			tuiWG:              &sync.WaitGroup{},
			agentNotifications: pubsub.NewBroker[notify.Notification](),
			runCompletions:     pubsub.NewBroker[notify.RunComplete](),
		},
		shutdownPhases: shutdownPhases{
			shutdownTimeout: defaultShutdownTimeout,
		},
		globalCtx: ctx,
	}
	app.app = app
	app.agentDispatcher = NewAgentDispatcher(app.globalCtx, func() agent.Coordinator { return app.AgentCoordinator }, app.agentNotifications, app.runCompletions)

	eventsCtx, cancel := context.WithCancel(ctx)
	app.eventsCtx = eventsCtx
	setupSubscriberMustDeliver(eventsCtx, app.serviceEventsWG, "permissions",
		app.permissions.Subscribe, app.events)
	setupSubscriberMustDeliver(eventsCtx, app.serviceEventsWG, "permissions-notifications",
		app.permissions.SubscribeNotifications, app.events)
	setupSubscriberMustDeliver(eventsCtx, app.serviceEventsWG, "question-batches",
		app.Questions.Subscribe, app.events)
	setupSubscriberMustDeliver(eventsCtx, app.serviceEventsWG, "question-notifications",
		app.Questions.SubscribeNotifications, app.events)
	setupSubscriber(eventsCtx, app.serviceEventsWG, "agent-notifications",
		app.agentNotifications.Subscribe, app.events)
	setupSubscriber(eventsCtx, app.serviceEventsWG, "run-completions",
		app.runCompletions.Subscribe, app.events)
	app.cleanupFuncs = append(app.cleanupFuncs, func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	})
	return app
}

// ShutdownForTest tears down resources registered by a synthetic App without
// invoking production services that its test doubles may not fully implement.
func (app *App) ShutdownForTest() {
	app.shutdownMu.Lock()
	if app.shutdownState == shutdownStateDone {
		app.shutdownMu.Unlock()
		return
	}
	if app.shutdownState == shutdownStateShuttingDown {
		done := app.shutdownDone
		app.shutdownMu.Unlock()
		<-done
		return
	}
	app.shutdownState = shutdownStateShuttingDown
	app.shutdownDone = make(chan struct{})
	cleanups := append([]func(context.Context) error(nil), app.cleanupFuncs...)
	app.cleanupFuncs = nil
	app.shutdownMu.Unlock()

	for _, cleanup := range cleanups {
		if cleanup != nil {
			_ = cleanup(context.Background())
		}
	}

	app.shutdownMu.Lock()
	app.shutdownState = shutdownStateDone
	close(app.shutdownDone)
	app.shutdownMu.Unlock()
}
