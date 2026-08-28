package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/rave-soft/sennit/internal/agent/notify"
	"github.com/rave-soft/sennit/internal/pubsub"
)

// appEvents groups the event fan-in App's subscribers (the TUI, `sennit
// run`, herdr) read from: the shared broker every service's own events are
// republished onto, the context/WaitGroup pair that bounds those fan-in
// goroutines, and the two purpose-built brokers (agent notifications and
// terminal run completions) that feed it. It is embedded anonymously in
// App, so every field here is promoted onto *App exactly as it was before
// this type existed (app.events, app.eventsCtx, app.agentNotifications,
// ...) — App is a facade over this plus appServices and shutdownPhases,
// not a new API.
type appEvents struct {
	serviceEventsWG *sync.WaitGroup
	eventsCtx       context.Context
	// events is typed any rather than bubbletea's tea.Msg: the app
	// package is core (no UI dependency), and any is what tea.Msg
	// aliases down to anyway. Consumers that need a tea.Msg (the TUI)
	// convert at the boundary; see appws.AppWorkspace.Subscribe.
	events *pubsub.Broker[any]
	tuiWG  *sync.WaitGroup

	agentNotifications *pubsub.Broker[notify.Notification]
	// runCompletions is the authoritative per-run completion signal,
	// emitted once per top-level agent turn after all message
	// updates have been flushed. Bridged into app.events so
	// subscribers (notably `sennit run`) can drive their exit on a
	// deterministic, payload-bearing event instead of guessing from
	// message finish parts.
	runCompletions *pubsub.Broker[notify.RunComplete]
}

// Events returns a per-caller subscription channel for application events.
// Each caller receives its own channel; all callers receive every event.
func (app *App) Events(ctx context.Context) <-chan pubsub.Event[any] {
	return app.events.Subscribe(ctx)
}

// SendEvent publishes a message to all event subscribers.
func (app *App) SendEvent(msg any) {
	app.events.Publish(pubsub.UpdatedEvent, msg)
}

// AgentNotifications returns the broker for agent notification events.
func (app *App) AgentNotifications() *pubsub.Broker[notify.Notification] {
	return app.agentNotifications
}

// RunCompletions returns the broker for the authoritative per-run
// terminal RunComplete events. The dispatcher ([AgentDispatcher.run])
// uses it to emit a reliable terminal event when a run fails before the
// coordinator could publish one of its own.
func (app *App) RunCompletions() *pubsub.Broker[notify.RunComplete] {
	return app.runCompletions
}

func (app *App) setupEvents() {
	ctx, cancel := context.WithCancel(app.globalCtx)
	app.eventsCtx = ctx
	setupSubscriber(ctx, app.serviceEventsWG, "sessions", app.sessions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "messages", app.messages.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions", app.permissions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "permissions-notifications", app.permissions.SubscribeNotifications, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-batches", app.Questions.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "question-notifications", app.Questions.SubscribeNotifications, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "history", app.History.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "agent-notifications", app.agentNotifications.Subscribe, app.events)
	setupSubscriberMustDeliver(ctx, app.serviceEventsWG, "run-completions", app.runCompletions.Subscribe, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "mcp", app.MCP.SubscribeEvents, app.events)
	setupSubscriber(ctx, app.serviceEventsWG, "lsp", app.lsp.SubscribeLSPEvents, app.events)
	if app.Skills != nil {
		setupSubscriber(ctx, app.serviceEventsWG, "skills", app.Skills.SubscribeEvents, app.events)
	}
	cleanupFunc := func(context.Context) error {
		cancel()
		app.serviceEventsWG.Wait()
		app.events.Shutdown()
		return nil
	}
	if err := app.AddCleanup(cleanupFunc); err != nil {
		slog.Error("Failed to register event cleanup", "error", err)
		_ = cleanupFunc(context.Background())
	}
}

func setupSubscriber[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[any],
) {
	// Subscribed here rather than inside the goroutine below. A broker
	// only delivers to subscribers already registered when a value is
	// published, so subscribing asynchronously leaves a window in which
	// everything the source emits is lost outright -- and the window
	// closes only when the runtime happens to schedule this goroutine.
	// With more than one P that is usually immediate, which is why it
	// took a single-P run to make it visible; the window is real either
	// way, and it sits at startup, exactly where the first events are.
	subCh := subscriber(ctx)
	wg.Go(func() {
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.Publish(pubsub.UpdatedEvent, any(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}

// ForwardEvents subscribes to an event source attached to app after
// construction (e.g. the thread manager, wired in post-bootstrap by
// SetDelegationManagers — see internal/app/threadspawn/attach.go) and
// forwards its events into app's own event fan-in (app.events), so
// app.Subscribe consumers see them exactly like every source wired in
// at New() time by setupEvents.
//
// A free function rather than a method because Go has no generic
// methods; T is inferred from subscribe at the call site. Exported
// because the caller lives in a different package (internal/thread, via
// internal/app/threadspawn). Callers must invoke this before
// app.Shutdown tears down app.eventsCtx/app.events — true in practice,
// since thread managers are attached once, early in a workspace's life.
func ForwardEvents[T any](app *App, name string, subscribe func(context.Context) <-chan pubsub.Event[T]) {
	setupSubscriber(app.eventsCtx, app.serviceEventsWG, name, subscribe, app.events)
}

// setupSubscriberMustDeliver is the bounded-blocking fan-in variant of
// setupSubscriber: it re-publishes upstream events onto the shared
// app.events broker using PublishMustDeliver instead of Publish. Use
// this for terminal events that subscribers cannot tolerate losing —
// notably RunComplete, which is the authoritative end-of-run signal
// for `sennit run`. A lossy fan-in here can drop the only terminal
// event and hang non-interactive clients waiting on it.
func setupSubscriberMustDeliver[T any](
	ctx context.Context,
	wg *sync.WaitGroup,
	name string,
	subscriber func(context.Context) <-chan pubsub.Event[T],
	broker *pubsub.Broker[any],
) {
	// Subscribed before the goroutine starts, for the reason
	// setupSubscriber gives -- and it matters more here: this variant
	// exists for events subscribers cannot tolerate losing.
	subCh := subscriber(ctx)
	wg.Go(func() {
		for {
			select {
			case event, ok := <-subCh:
				if !ok {
					slog.Debug("Subscription channel closed", "name", name)
					return
				}
				broker.PublishMustDeliver(ctx, pubsub.UpdatedEvent, any(event))
			case <-ctx.Done():
				slog.Debug("Subscription cancelled", "name", name)
				return
			}
		}
	})
}
