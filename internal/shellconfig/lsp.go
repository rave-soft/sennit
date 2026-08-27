package shellconfig

import (
	"context"
	"io"
	"log/slog"
)

// handleLSP implements the `lsp` builtin.
//
// Usage:
//
//	lsp add <name> --command CMD [--args ARG ...] [--env KEY VALUE ...]
//	    [--filetypes TYPE ...] [--root-markers MARKER ...]
//	    [--timeout N] [--disabled true|false]
//	    [--init-options JSON] [--options JSON]
//	lsp remove <name>   (alias: rm)
//
// "add" defines or updates an LSP server; repeated calls with the same <name>
// update the same entry. "remove" deletes it.
func handleLSP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return dispatchAddRemove(ctx, args, stderr,
		"lsp", "usage: lsp add <name> --command CMD [flags] | lsp remove <name>",
		lspAdd, lspRemove)
}

// lspAddFlags is the declarative flag surface for `lsp add`.
var lspAddFlags = []flagSpec{
	{name: "--command", jsonKey: "command", kind: flagString, op: opSet},
	{name: "--args", jsonKey: "args", kind: flagString, op: opAppend},
	{name: "--env", child: "env", kind: flagKeyValue, op: opSetChild},
	{name: "--filetypes", jsonKey: "filetypes", kind: flagString, op: opAppend},
	{name: "--root-markers", jsonKey: "root_markers", kind: flagString, op: opAppend},
	{name: "--timeout", jsonKey: "timeout", kind: flagInt, op: opSet},
	{name: "--disabled", jsonKey: "disabled", kind: flagBool, op: opSet},
	{name: "--init-options", jsonKey: "init_options", kind: flagJSONAny, op: opSet},
	{name: "--options", jsonKey: "options", kind: flagJSONAny, op: opSet},
}

func lspAdd(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: lsp add <name> --command CMD [--args ARG ...] [--env KEY VALUE ...] [--filetypes TYPE ...] [--root-markers MARKER ...] [--timeout N] [--disabled true|false] [--init-options JSON] [--options JSON]")
	}
	name := args[2]
	slog.Info("LSP server defined in shell config", "name", name)
	l := b.addLocal(b.section("lsp"), "lsp", name)

	if err := applyFlags(lspAddFlags, args, 3, l, "lsp add", stderr); err != nil {
		return err
	}

	slog.Debug("LSP recorded", "name", name)
	return nil
}

func lspRemove(b *ConfigBuilder, args []string, stderr io.Writer) error {
	if len(args) < 3 {
		return usage(stderr, "usage: lsp remove <name>")
	}
	removeNamedEntry(b, "lsp", "LSP server", args[2])
	return nil
}
