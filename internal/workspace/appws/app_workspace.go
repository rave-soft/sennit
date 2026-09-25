package appws

import (
	"sync/atomic"

	"github.com/rave-soft/sennit/internal/app"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/shell"
	"github.com/rave-soft/sennit/internal/workspace"
)

// runAndCaptureStream and runAndPersist are indirected through package
// vars, rather than called directly on shell.*, so a test can substitute
// a failing stand-in without needing a shell command that actually fails
// to start (RunAndCaptureStream folds every real outcome — a normal
// exit, a cancellation, or a command that never ran — into CaptureResult
// and never itself returns a non-nil error).
var (
	runAndCaptureStream = shell.RunAndCaptureStream
	runAndPersist       = shell.RunAndPersist
)

// AppWorkspace implements the Workspace interface by delegating
// directly to an in-process [app.App] instance.
//
// Its methods are grouped by role into sibling files named after the
// Workspace role interface they implement (app_workspace_sessions.go,
// app_workspace_agent.go, app_workspace_mcp.go, ...); this file keeps
// only the type itself, its constructor, and the two accessors below
// that are not part of the Workspace interface at all.
type AppWorkspace struct {
	app   *app.App
	store *config.ConfigStore

	worktreeRoot *AppWorkspace
	worktreeName string
	worktreePath string

	// frontendConfigCache memoizes Config()'s *workspace.FrontendConfig
	// against the *config.Config pointer it was built from - see
	// app_workspace_config.go's Config() doc comment. An atomic.Pointer
	// because Config() runs from the Update goroutine and from tea.Cmd
	// goroutines concurrently.
	frontendConfigCache atomic.Pointer[frontendConfigCacheEntry]
}

// NewAppWorkspace creates a new AppWorkspace wrapping the given app
// and config store.
func NewAppWorkspace(a *app.App, store *config.ConfigStore) *AppWorkspace {
	return &AppWorkspace{
		app:   a,
		store: store,
	}
}

// App returns the underlying app.App instance.
func (w *AppWorkspace) App() *app.App {
	return w.app
}

// Store returns the underlying config store.
func (w *AppWorkspace) Store() *config.ConfigStore {
	return w.store
}

// Compile-time check that AppWorkspace implements Workspace.
var _ workspace.Workspace = (*AppWorkspace)(nil)
