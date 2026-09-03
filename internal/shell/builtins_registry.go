package shell

import (
	"context"
	"io"
)

// BuiltinHandler is a function that handles a shell builtin command.
// It receives the full args slice (including the command name as args[0]),
// the context (which may carry a ConfigBuilder), and I/O streams.
type BuiltinHandler func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error

// builtinEntry pairs a handler with the condition under which it should
// intercept its name. jq is active unconditionally; a config builtin
// (provider, model, mcp, ...) is active only while a sennitrc script is
// loading, so a same-named program on PATH is not shadowed during
// ordinary command execution.
type builtinEntry struct {
	handler BuiltinHandler
	active  func(ctx context.Context) bool
}

// builtins holds all in-process builtin handlers, including jq (registered
// in this package) and config builtins (registered by shellconfig via
// RegisterBuiltin/RegisterConditionalBuiltin to avoid an import cycle).
// Dispatched by builtinHandler in run.go.
var builtins = make(map[string]builtinEntry)

func init() {
	RegisterBuiltin("jq", handleJQ)
}

// alwaysActive is the active func for a builtin that always intercepts its
// name, regardless of context.
func alwaysActive(context.Context) bool { return true }

// RegisterBuiltin registers a builtin command handler that is always
// active. Must be called during init(), before any shell execution. If a
// handler is already registered for the same name, the new one replaces
// it.
func RegisterBuiltin(name string, handler BuiltinHandler) {
	RegisterConditionalBuiltin(name, handler, alwaysActive)
}

// RegisterConditionalBuiltin registers a builtin command handler that
// intercepts its name only when active(ctx) reports true; when it doesn't,
// dispatch falls through to the real exec path, so a program of the same
// name on PATH still runs. Must be called during init(), before any shell
// execution. If a handler is already registered for the same name, the
// new one replaces it.
func RegisterConditionalBuiltin(name string, handler BuiltinHandler, active func(context.Context) bool) {
	builtins[name] = builtinEntry{handler: handler, active: active}
}
