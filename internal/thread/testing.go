package thread

import "github.com/rave-soft/braid/internal/message"

// NewTaskManagerForTest constructs a TaskManager sharing mgr's own
// lifecycle and context — exactly the wiring Attach performs in
// production (see attachWithDeps) — for tests in other packages that need
// a real, working TaskManager without Attach's global-DB-dir/git-toplevel
// dependencies (which assume a real project checkout and a shared
// process-wide database, both awkward in an isolated test).
//
// This is test-only scaffolding, not a second production wiring path —
// mirrors app.NewForTest's and backend.InsertWorkspaceForTest's naming and
// purpose. It exists because NewTaskManager itself requires mgr's
// unexported lc/ctx fields and so can only be called from within this
// package; every other caller either is this package (attach.go) or goes
// through here.
func NewTaskManagerForTest(mgr *Manager, spawner Spawner, messages message.Service) *TaskManager {
	return NewTaskManager(mgr.store, spawner, messages, mgr.lc, mgr.ctx)
}
