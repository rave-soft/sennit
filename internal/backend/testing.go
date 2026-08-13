package backend

import (
	"context"

	"github.com/rave-soft/braid/internal/agent"
	"github.com/rave-soft/braid/internal/app"
)

// InsertWorkspaceForTest registers ws with b under its current ID and
// path. It is intended for tests in other packages that need to drive
// HTTP handlers against a synthetic workspace without booting a real
// app.App. Production code should go through CreateWorkspace.
//
// If the workspace has no run context yet it is derived from the
// backend context (falling back to context.Background), mirroring the
// initialization CreateWorkspace performs, so dispatched agent runs
// have a non-nil ws.ctx. Likewise, if ws.dispatcher is nil it is built
// here exactly as createWorkspace builds it — same lazy coordinator
// getter, bound to ws.ctx — after the ctx back-fill above, so a
// hand-built Workspace looks like a real one to SendMessage instead of
// leaving it with a nil dispatcher.
func InsertWorkspaceForTest(b *Backend, ws *Workspace) {
	if ws.resolvedPath == "" {
		ws.resolvedPath = ws.Path
	}
	if ws.clients == nil {
		ws.clients = make(map[string]*clientState)
	}
	if ws.ctx == nil {
		parent := b.ctx
		if parent == nil {
			parent = context.Background()
		}
		ws.ctx, ws.cancel = context.WithCancel(parent)
	}
	// AgentNotifications/RunCompletions are promoted methods on the
	// embedded *app.App; some synthetic workspaces (e.g. multiclient_test.go)
	// have no App at all, so guard against calling them on a nil
	// receiver. Such a workspace has no coordinator either, so leaving
	// dispatcher nil here is correct: SendMessage's own nil-dispatcher
	// guard turns that into ErrAgentNotInitialized instead of a panic.
	if ws.dispatcher == nil && ws.App != nil {
		ws.dispatcher = app.NewAgentDispatcher(ws.ctx, func() agent.Coordinator { return ws.AgentCoordinator }, ws.AgentNotifications(), ws.RunCompletions())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.workspaces.Set(ws.ID, ws)
	if ws.resolvedPath != "" {
		b.pathIndex[ws.resolvedPath] = ws.ID
	}
}

// RegisterClientForTesting installs a creation hold for clientID on
// ws using the backend's normal registerClient path. Intended for
// tests in other packages that need to drive a hold-only client
// (streams == 0) without booting a real CreateWorkspace flow.
func RegisterClientForTesting(b *Backend, ws *Workspace, clientID string) error {
	if _, err := validateClientID(clientID); err != nil {
		return err
	}
	b.registerClient(ws, clientID)
	return nil
}

// SetWorkspaceShutdownFnForTest overrides the workspace teardown
// callback. Useful for tests in other packages that drive synthetic
// workspaces (where the embedded [app.App] is incomplete) through
// detach paths that would otherwise crash inside App.Shutdown.
func SetWorkspaceShutdownFnForTest(ws *Workspace, fn func()) {
	ws.shutdownFn = fn
}

// WorkspaceLiveStreamCountForTest returns the number of clients on ws
// that have at least one live SSE stream. Used by integration tests
// in other packages to wait for SSE attaches before publishing events.
func WorkspaceLiveStreamCountForTest(ws *Workspace) int {
	ws.clientsMu.Lock()
	defer ws.clientsMu.Unlock()
	n := 0
	for _, cs := range ws.clients {
		if cs.streams > 0 {
			n++
		}
	}
	return n
}
