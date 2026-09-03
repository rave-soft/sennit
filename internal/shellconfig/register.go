// Package shellconfig implements the Bash-powered config format for Sennit.
//
// It provides shell builtins (provider, model, mcp, lsp, permissions, hook,
// option) that populate config by mutating a ConfigBuilder
// stored on the shell context. The builtins are registered at init time via
// shell.RegisterConditionalBuiltin, active only while a ConfigBuilder is on
// the context (a sennitrc load in progress); during normal bash tool or
// hook execution they are inactive, so a same-named program on PATH — the
// `mcp` CLI, say — runs instead of being shadowed.
//
// This package sits between shell and config: it imports shell (for
// RegisterConditionalBuiltin and Run), and config imports shellconfig to
// run sennitrc files.
package shellconfig

import (
	"context"
	"fmt"
	"io"

	"github.com/rave-soft/sennit/internal/shell"
)

// hasConfigBuilder is the active condition shared by every config builtin:
// only intercept the name while a sennitrc script is loading.
func hasConfigBuilder(ctx context.Context) bool {
	return configBuilderFromCtx(ctx) != nil
}

func init() {
	shell.RegisterConditionalBuiltin("provider", handleProvider, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("model", handleModel, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("mcp", handleMCP, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("lsp", handleLSP, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("permissions", handlePermissions, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("hook", handleHook, hasConfigBuilder)
	shell.RegisterConditionalBuiltin("option", handleOption, hasConfigBuilder)
}

// usage prints a usage message to stderr and returns an error.
func usage(stderr io.Writer, msg string) error {
	fmt.Fprintln(stderr, msg)
	return fmt.Errorf("%s", msg)
}

// appendArr appends value to the []any slice stored at m[key], creating it
// if needed, and returns the result. Shared by the builtins' own array
// flags and applyFlags' opAppend (flags.go), both of which store
// list-valued config properties the same way.
func appendArr(m map[string]any, key string, value any) []any {
	arr, _ := m[key].([]any)
	return append(arr, value)
}
