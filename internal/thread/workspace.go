package thread

import (
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/question"
)

// Workspace is the domain-facing slice of the App hosting a delegation's
// workspace that this package drives: the coordinator runs are dispatched
// into, the session/message services a delegation's child session and
// completion notes live in, the permission service whose traffic a
// delegation's run raises, and the event stream a delegation workspace's
// prompts are relayed onto.
//
// It is declared here, on the consumer side, as the narrowest set of
// capabilities this package actually calls — the import-direction
// counterpart of the Spawner/Handle seam: internal/thread must not import
// internal/app (the composition root that imports thread's types) nor
// internal/agent (whose own closure pulls in internal/config and
// internal/db), so an App is handed in as a [Handle] by whichever spawner
// produced it, and each accessor returns this package's own narrow view.
// The composition seam (internal/app/threadspawn) adapts the real
// *app.App — its agent.Coordinator to [Coordinator], its notify.RunComplete
// broker to [RunCompletionBroker], its session/message services to
// [SessionService]/[MessageService] — so no app- or agent-specific type
// crosses into this package.
type Workspace interface {
	// Coordinator is the agent coordinator the delegation's runs are
	// dispatched through (and, for a thread's parent, where its
	// completion is delivered and a mid-run ask is looked up). May be
	// nil for an App that never initialized one; callers guard it.
	Coordinator() Coordinator
	// Sessions is the service the delegation's child session is created
	// in.
	Sessions() SessionService
	// Messages is the service a delegation's completion notes are
	// persisted into (the parent session's history).
	Messages() MessageService
	// Permissions is the service whose traffic the delegation workspace
	// raises.
	Permissions() permission.Service
	// Questions is the service a delegation's question tool calls raise.
	// Like Permissions, it blocks the caller until answered, so a thread's
	// prompts need the same relay into the parent's event stream (see
	// lifecycle.forwardQuestions).
	Questions() question.Service
	// RunCompletions is the broker the lifecycle's per-run watcher
	// subscribes to, so a dispatched run's terminal event is picked up
	// here rather than polled.
	RunCompletions() RunCompletionBroker
	// SendEvent publishes onto the workspace's application event stream;
	// the parent workspace's stream is where a relayed prompt must land
	// for the user to see and answer it.
	SendEvent(msg any)
}
