// Package shellconfig implements the Bash-powered config format for Sennit.
//
// It provides shell builtins (provider, model, mcp, lsp, permissions, hook,
// option) that populate config by mutating a ConfigBuilder
// stored on the shell context. The builtins are registered at init time via
// shell.RegisterBuiltin and are gated by the ConfigBuilder on the context —
// they are no-ops during normal bash tool execution.
//
// This package sits between shell and config: it imports shell (for
// RegisterBuiltin and Run), and config imports shellconfig to run sennitrc
// files.
package shellconfig

import (
	"fmt"
	"io"

	"github.com/rave-soft/sennit/internal/shell"
)

func init() {
	shell.RegisterBuiltin("provider", handleProvider)
	shell.RegisterBuiltin("model", handleModel)
	shell.RegisterBuiltin("mcp", handleMCP)
	shell.RegisterBuiltin("lsp", handleLSP)
	shell.RegisterBuiltin("permissions", handlePermissions)
	shell.RegisterBuiltin("hook", handleHook)
	shell.RegisterBuiltin("option", handleOption)
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
