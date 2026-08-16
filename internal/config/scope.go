package config

import "fmt"

// Scope determines which config file is targeted for read/write operations.
type Scope int

const (
	// ScopeGlobal targets the global data config (~/.local/share/sennit/sennit.json).
	ScopeGlobal Scope = iota
	// ScopeWorkspace targets the workspace config (.sennit/sennit.json).
	ScopeWorkspace
)

// String returns a human-readable label for the scope.
func (s Scope) String() string {
	switch s {
	case ScopeGlobal:
		return "global"
	case ScopeWorkspace:
		return "workspace"
	default:
		return fmt.Sprintf("Scope(%d)", int(s))
	}
}

// ErrNoWorkspaceConfig is returned when a workspace-scoped write is
// attempted on a ConfigStore that has no workspace config path.
var ErrNoWorkspaceConfig = fmt.Errorf("no workspace config path configured")

// ErrNoGlobalConfig is returned when a global-scoped write is attempted on a
// ConfigStore with no global data path configured (e.g. a bare ConfigStore
// built directly in a test, as opposed to one produced by Load). Guarding
// this avoids atomicWrite silently resolving an empty path to a stray file
// in the process's current directory.
var ErrNoGlobalConfig = fmt.Errorf("no global config path configured")
