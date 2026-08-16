package agent

import (
	"context"

	"github.com/rave-soft/sennit/internal/message"
)

// OriginAgent marks a prompt raised by an agent on the person's behalf
// (a delegation's goal or a thread_send/task_send follow-up) rather than
// typed by the person. It is the thread-domain spelling of
// message.OriginAgent and exists here so internal/thread can tag its own
// dispatches without importing internal/message (which pulls in
// internal/db): the value carries the same wire string, and
// [coordinator.run] converts it back to a message.Origin at the seam
// where the prompt is persisted.
const OriginAgent = message.OriginAgent

// WithAgentDispatch returns ctx tagged so the prompt dispatched through
// it is persisted with Origin message.OriginAgent instead of the default
// message.OriginPerson. It tags the same context key used elsewhere for
// prompt origin, so [coordinator.run] reads it through the unchanged
// [PromptOriginFromContext] path and persistence behavior is identical.
// It is still persisted with Role == message.User and reaches the model
// exactly like any other user turn (Origin has zero effect on what the
// model sees — see Message.ToAIMessage), but the UI can use it to mark
// the message as not the person's own words.
func WithAgentDispatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, promptOriginContextKey{}, OriginAgent)
}
