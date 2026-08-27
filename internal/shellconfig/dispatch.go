package shellconfig

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

// dispatchAddRemove implements the shared "add | remove (alias rm)"
// subcommand shape every builtin that manages a named collection (hook,
// lsp, mcp, provider) follows: resolve the ConfigBuilder from ctx, bail out
// quietly when none is present (normal bash tool execution), require a
// subcommand, and route "add" / "remove"|"rm" to the caller's handlers.
//
// name identifies the builtin in the "unknown subcommand" error, and
// usageMsg is printed verbatim when no subcommand is given, so each
// builtin keeps its own full usage string.
func dispatchAddRemove(
	ctx context.Context,
	args []string,
	stderr io.Writer,
	name, usageMsg string,
	add, remove func(*ConfigBuilder, []string, io.Writer) error,
) error {
	b := configBuilderFromCtx(ctx)
	if b == nil {
		return nil
	}
	if len(args) < 2 {
		return usage(stderr, usageMsg)
	}

	switch args[1] {
	case "add":
		return add(b, args, stderr)
	case "remove", "rm":
		return remove(b, args, stderr)
	default:
		return usage(stderr, fmt.Sprintf("%s: unknown subcommand %q (expected add or remove)", name, args[1]))
	}
}

// removeNamedEntry implements the "remove <name>" body shared by lspRemove
// and mcpRemove: both tombstone the named entry in the given section and
// log the removal, differing only in the section key and the noun used in
// the log message.
func removeNamedEntry(b *ConfigBuilder, section, noun, name string) {
	b.removeLocal(b.section(section), section, name)
	slog.Info(noun+" removed in shell config", "name", name)
}

// filterOutByField returns a copy of arr with every map[string]any entry
// whose field equals value removed; entries that are not a
// map[string]any, or whose field does not match, are kept in order. It is
// the shared body of the "drop the entry with this id/name, keep the
// rest" filtering used when removing a hook by name or replacing/removing
// a model by id.
func filterOutByField(arr []any, field string, value any) []any {
	kept := make([]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok && m[field] == value {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}
