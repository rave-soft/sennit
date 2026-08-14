package agent

import (
	"context"

	"github.com/rave-soft/braid/internal/message"
)

// promptOriginContextKey is the unexported context key used to carry the
// message.Origin a dispatched prompt should be persisted under, from
// thread/task creation (and Manager.Send/TaskManager.Send) down into
// coordinator.run without forcing a breaking change to the
// Coordinator.Run / RunAccepted signatures. It mirrors runIDContextKey /
// WithRunID / RunIDFromContext.
type promptOriginContextKey struct{}

// WithPromptOrigin returns ctx tagged so the prompt dispatched through it
// is persisted with the given message.Origin instead of the default
// message.OriginPerson. Used by thread/task dispatch to mark a
// delegation's goal or a thread_send/task_send follow-up as
// message.OriginAgent: it is still persisted with Role == message.User and
// reaches the model exactly like any other user turn (Origin has zero
// effect on what the model sees - see Message.ToAIMessage), but the UI can
// use it to mark the message as not the person's own words.
func WithPromptOrigin(ctx context.Context, origin message.Origin) context.Context {
	return context.WithValue(ctx, promptOriginContextKey{}, origin)
}

// PromptOriginFromContext returns the origin set by [WithPromptOrigin], or
// "" if none was set. Exported because the coordinator needs to read it;
// safe to call on any context. An empty result means "caller did not tag
// this dispatch"; downstream code treats that as message.OriginPerson.
func PromptOriginFromContext(ctx context.Context) message.Origin {
	if v, ok := ctx.Value(promptOriginContextKey{}).(message.Origin); ok {
		return v
	}
	return ""
}
