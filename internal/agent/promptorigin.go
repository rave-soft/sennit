package agent

import (
	"context"

	"github.com/rave-soft/sennit/internal/message"
)

// promptOriginContextKey is the unexported context key used to carry the
// message.Origin a dispatched prompt should be persisted under, from
// thread/task creation (and Manager.Send/TaskManager.Send) down into
// coordinator.run without forcing a breaking change to the
// Coordinator.Run / RunAccepted signatures. It mirrors runIDContextKey /
// WithRunID / RunIDFromContext.
type promptOriginContextKey struct{}

// PromptOriginFromContext returns the origin set by a previous prompt
// dispatch (see threadorigin.go's promptOriginContextKey usage), or
// "" if none was set. Exported because the coordinator needs to read it;
// safe to call on any context. An empty result means "caller did not tag
// this dispatch"; downstream code treats that as message.OriginPerson.
func PromptOriginFromContext(ctx context.Context) message.Origin {
	if v, ok := ctx.Value(promptOriginContextKey{}).(message.Origin); ok {
		return v
	}
	return ""
}
