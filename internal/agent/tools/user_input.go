package tools

import "context"

type userInputContextKey string

// UserInputContextKey carries a function that reports when the person has
// something to say to this session. See [WaitForUserInput].
const UserInputContextKey userInputContextKey = "user_input"

// UserInputFunc returns a channel that closes once a prompt from the
// person (not another agent) is queued for the running session. Each call
// returns the channel for the next message, so a tool that has already
// been interrupted once can arm itself again.
type UserInputFunc func() <-chan struct{}

// WithUserInput attaches the signal to a run's context. The agent installs
// it when a turn starts; a context without one simply never reports input,
// which is the right behaviour for tools running outside a session.
func WithUserInput(ctx context.Context, fn UserInputFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, UserInputContextKey, fn)
}

// WaitForUserInput returns a channel that closes when the person sends a
// message to this session, or nil when the context carries no such signal.
// A prompt another agent queued (a delegation follow-up, say) does not
// close it — only the person's own words do, because cutting a wait short
// is an interactive "wait, do this instead" correction, not a way for one
// agent to derail another agent's work in flight (the same doctrine as
// agent.WithSteering and thread.lifecycle's steer).
//
// It exists for tools that spend their time waiting on something other than
// the person — a background thread finishing, say. Such a tool holds the
// turn open while doing nothing, so a message typed meanwhile sits in the
// queue until the wait ends, and the conversation appears to hang.
// Selecting on this channel lets the tool cut its wait short and hand the
// turn back, so the person is answered now and whatever was being waited
// for reports itself later.
//
// It says nothing about tools that are busy working: interrupting those
// would throw away the work, which is not what a new message asks for. A
// detachable foreground delegation (coordinator.runSubAgent, gated
// behind subAgentParams.Detachable) is the one exception: the work is
// not thrown away, it is detached — the child run keeps going on its
// own recovered context, and this signal only tells the parent's tool
// call to stop blocking on it and hand the turn back, with the result
// delivered later through the completion inbox.
func WaitForUserInput(ctx context.Context) <-chan struct{} {
	fn := getContextValue[UserInputFunc](ctx, UserInputContextKey, nil)
	if fn == nil {
		return nil
	}
	return fn()
}
