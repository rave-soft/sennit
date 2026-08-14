package thread

// NewTaskManagerForTest constructs a TaskManager sharing mgr's own
// lifecycle and context — exactly the wiring threadspawn.Attach performs
// in production — for callers outside this package that need a real,
// working TaskManager (threadspawn itself for the production wiring, and
// tests in other packages that want one without Attach's global-DB-dir
// and git-toplevel dependencies, which assume a real project checkout and
// a shared process-wide database, both awkward in an isolated test).
//
// It exists because NewTaskManager itself requires mgr's unexported
// lc/ctx fields and so can only be called from within this package; every
// other caller — production and test alike — goes through here.
func NewTaskManagerForTest(mgr *Manager, spawner Spawner, messages MessageService) *TaskManager {
	return NewTaskManager(mgr.store, spawner, messages, mgr.lc, mgr.ctx)
}

// PublishForTest emits a lifecycle event through the manager's own event
// broker, for tests outside this package that need to drive the event
// stream (production emits only through real lifecycle transitions, which
// are hard to reach from the composition seam). It publishes through the
// manager's lifecycle so the event is indistinguishable from a real one.
func (m *Manager) PublishForTest(t EventType, st Thread) {
	m.lc.publish(t, st)
}

// ParentSessionIDForTest exposes the in-memory parent-session link a
// delegation reports its completion to. It exists so tests outside this
// package can prove the link survives the path that created it — the
// field itself stays unexported because nothing in production reads it
// from outside the manager.
func (m *Manager) ParentSessionIDForTest(id string) string {
	c := m.lc.existingControl(id)
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.parentSessionID
}
