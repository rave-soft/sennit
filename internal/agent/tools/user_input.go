package tools

import "context"

type userInputContextKey string

// UserInputContextKey carries a function that reports when the user has
// something to say to this session. See [WaitForUserInput].
const UserInputContextKey userInputContextKey = "user_input"

// UserInputFunc returns a channel that closes once a prompt from the user
// is queued for the running session. Each call returns the channel for the
// next message, so a tool that has already been interrupted once can arm
// itself again.
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

// WaitForUserInput returns a channel that closes when the user sends a
// message to this session, or nil when the context carries no such signal.
//
// It exists for tools that spend their time waiting on something other than
// the user — a background thread finishing, say. Such a tool holds the turn
// open while doing nothing, so a message typed meanwhile sits in the queue
// until the wait ends, and the conversation appears to hang. Selecting on
// this channel lets the tool cut its wait short and hand the turn back, so
// the user is answered now and whatever was being waited for reports itself
// later.
//
// It says nothing about tools that are busy working: interrupting those
// would throw away the work, which is not what a new message asks for.
func WaitForUserInput(ctx context.Context) <-chan struct{} {
	fn := getContextValue[UserInputFunc](ctx, UserInputContextKey, nil)
	if fn == nil {
		return nil
	}
	return fn()
}
