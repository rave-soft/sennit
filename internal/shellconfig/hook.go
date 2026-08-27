package shellconfig

import (
	"context"
	"io"
	"log/slog"
)

// handleHook implements the `hook` builtin.
//
// Usage:
//
//	hook add <event> --command CMD [--name NAME] [--matcher REGEX] [--timeout N]
//	hook remove <event> [--name NAME]   (alias: rm)
//
// "add" appends a hook to the event's list; multiple hooks per event
// accumulate. "remove" drops the named hook(s) from the event, or clears the
// whole event when no --name is given. Only named hooks can be removed
// individually; give a hook a --name if you intend to remove it later.
func handleHook(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return dispatchAddRemove(ctx, args, stderr,
		"hook", "usage: hook add <event> --command CMD [flags] | hook remove <event> [--name NAME]",
		hookAdd, hookRemove)
}

// hookAddFlags is the declarative flag surface for `hook add`.
var hookAddFlags = []flagSpec{
	{name: "--command", jsonKey: "command", kind: flagString, op: opSet},
	{name: "--matcher", jsonKey: "matcher", kind: flagString, op: opSet},
	{name: "--timeout", jsonKey: "timeout", kind: flagInt, op: opSet},
	{name: "--name", jsonKey: "name", kind: flagString, op: opSet},
}

func hookAdd(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: hook add <event> --command CMD [--name NAME] [--matcher REGEX] [--timeout N]")
	}
	event := args[2]
	slog.Info("Hook defined in shell config", "event", event)
	h := map[string]any{}

	if err := applyFlags(hookAddFlags, args, 3, h, "hook add", stderr); err != nil {
		return err
	}

	if _, ok := h["command"]; !ok {
		return usage(stderr, "hook add: --command is required")
	}

	hooks := b.section("hooks")
	arr, _ := hooks[event].([]any)
	hooks[event] = append(arr, h)

	slog.Debug("Hook recorded", "event", event)
	return nil
}

// hookRemoveFlags is the declarative flag surface for `hook remove`.
var hookRemoveFlags = []flagSpec{
	{name: "--name", jsonKey: "name", kind: flagString, op: opSet},
}

func hookRemove(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: hook remove <event> [--name NAME]")
	}
	event := args[2]

	flags := map[string]any{}
	if err := applyFlags(hookRemoveFlags, args, 3, flags, "hook remove", stderr); err != nil {
		return err
	}
	name, _ := flags["name"].(string)

	hooks := b.section("hooks")

	// No name: clear every hook for the event.
	if name == "" {
		delete(hooks, event)
		slog.Info("Hooks cleared in shell config", "event", event)
		return nil
	}

	// Name given: drop matching hooks, keeping the rest.
	arr, _ := hooks[event].([]any)
	hooks[event] = filterOutByField(arr, "name", name)

	slog.Info("Hook removed in shell config", "event", event, "name", name)
	return nil
}
