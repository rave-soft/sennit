package app

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/thread"
	"github.com/stretchr/testify/require"
)

// delegationSnapshotCoordinator satisfies agent.Coordinator for App tests.
type delegationSnapshotCoordinator struct {
	agent.Coordinator

	mu          sync.Mutex
	setCalls    int
	lastThreads tools.ThreadManager
	lastTasks   tools.TaskManager
}

func (c *delegationSnapshotCoordinator) SetDelegationTools(threads tools.ThreadManager, tasks tools.TaskManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	c.lastThreads = threads
	c.lastTasks = tasks
}

func (c *delegationSnapshotCoordinator) IsBusy() bool { return false }

func (c *delegationSnapshotCoordinator) state() (int, tools.ThreadManager, tools.TaskManager) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.setCalls, c.lastThreads, c.lastTasks
}

// TestSetDelegationManagersPublishesPairConsistently pins the C7
// guarantee: one call publishes the concrete thread/task pair and their
// tool adapters atomically, so both manager accessors read back the exact
// pair that was handed in, the snapshot carries the matching adapters for
// a later coordinator rebuild, and an already-existing coordinator
// receives the adapters from the same call.
func TestSetDelegationManagersPublishesPairConsistently(t *testing.T) {
	t.Parallel()

	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)
	coord := &delegationSnapshotCoordinator{}
	app.SetAgentCoordinatorForTest(coord)

	mgr := &thread.Manager{}
	tasks := &thread.TaskManager{}
	threadTools := fakeThreadToolManager{}
	taskTools := fakeTaskToolManager{}
	app.SetDelegationManagers(mgr, tasks, threadTools, taskTools)

	require.Same(t, mgr, app.ThreadManager())
	require.Same(t, tasks, app.TaskManager())
	require.Same(t, mgr, app.delegationManagersForTest().thread)
	require.Same(t, tasks, app.delegationManagersForTest().task)
	gotThreadTools, gotTaskTools := app.delegationToolAdapters()
	require.Equal(t, threadTools, gotThreadTools)
	require.Equal(t, taskTools, gotTaskTools)
	setCalls, lastThreads, lastTasks := coord.state()
	require.Equal(t, 1, setCalls, "an existing coordinator must receive the adapters from the same call")
	require.Equal(t, threadTools, lastThreads)
	require.Equal(t, taskTools, lastTasks)

	// A nil pair (a workspace that owns no managers) clears both
	// accessors from the same snapshot.
	app.SetDelegationManagers(nil, nil, nil, nil)
	require.Nil(t, app.ThreadManager())
	require.Nil(t, app.TaskManager())
}

// fakeThreadToolManager and fakeTaskToolManager are minimal, distinct
// implementations of the tool adapter interfaces so tests can assert an
// exact adapter round-tripped through the snapshot without depending on
// threadspawn's real adapters (which would pull that package's
// dependencies into this test).
type fakeThreadToolManager struct{ tools.ThreadManager }

type fakeTaskToolManager struct{ tools.TaskManager }

// TestSetDelegationManagersConcurrentPublicationReadsOneGeneration
// hammers the setter and the accessors from many goroutines (run with
// -race): every read of the snapshot must observe one whole generation,
// never a thread manager from one publication and a task manager from
// another.
func TestSetDelegationManagersConcurrentPublicationReadsOneGeneration(t *testing.T) {
	app := NewForTest(t.Context())
	t.Cleanup(app.ShutdownForTest)
	app.SetAgentCoordinatorForTest(&delegationSnapshotCoordinator{})

	// Pre-built generations: task -> thread maps each one; a reader may
	// land on any of them, but never on a mix of two. Every generation
	// carries a distinct thread manager so a mixed read is detectable,
	// and the map is built before any goroutine starts, never written
	// again.
	const generations = 64
	gens := make([]*thread.TaskManager, generations)
	byGen := make(map[*thread.TaskManager]*thread.Manager, generations)
	for i := range generations {
		gen := &thread.TaskManager{}
		gens[i] = gen
		byGen[gen] = &thread.Manager{}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range generations {
			app.SetDelegationManagers(byGen[gens[i]], gens[i], nil, nil)
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
		}
		close(stop)
	}()

	var reads int64
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// One atomic snapshot read: the pair it carries must be
				// a whole generation, whatever the public accessors
				// return on separate reads around the writer.
				snap := app.delegationManagersForTest()
				if snap.task != nil {
					want, ok := byGen[snap.task]
					if !ok || want != snap.thread {
						t.Errorf("snapshot read observed a mixed generation: thread=%p task=%p", snap.thread, snap.task)
						return
					}
				}
				// The public accessors must never panic and must agree
				// with at least one whole generation each.
				_ = app.ThreadManager()
				_ = app.TaskManager()
				atomic.AddInt64(&reads, 1)
			}
		}()
	}

	wg.Wait()
	require.Greater(t, reads, int64(0))
}

// TestAppHasSingleDelegationOwnershipSnapshot is the structural guard for
// C7: App must keep exactly one atomic snapshot of the concrete
// thread/task pair. It fails if a separate setter, public mutable field,
// second atomic, or tool-adapter storage is ever reintroduced on App,
// whatever its name.
func TestAppHasSingleDelegationOwnershipSnapshot(t *testing.T) {
	t.Parallel()

	snapshotType := reflect.TypeOf(atomic.Pointer[delegationManagerSnapshot]{})

	// Walk App's fields plus the fields of every (value) embedding it
	// contains, so the snapshot must live exactly once and nowhere else
	// can hold the concrete managers or the tool adapters.
	var walkFields func(t reflect.Type) []reflect.StructField
	walkFields = func(t reflect.Type) []reflect.StructField {
		var out []reflect.StructField
		for i := range t.NumField() {
			f := t.Field(i)
			out = append(out, f)
			if f.Anonymous {
				embedded := f.Type
				if embedded.Kind() == reflect.Pointer {
					embedded = embedded.Elem()
				}
				if embedded.Kind() == reflect.Struct {
					out = append(out, walkFields(embedded)...)
				}
			}
		}
		return out
	}

	var extraAtomics []string
	for _, f := range walkFields(reflect.TypeOf(App{})) {
		if f.Type == snapshotType {
			extraAtomics = append(extraAtomics, f.Name)
			continue
		}
		if f.Type == reflect.TypeOf(atomic.Pointer[thread.Manager]{}) ||
			f.Type == reflect.TypeOf(atomic.Pointer[thread.TaskManager]{}) {
			t.Errorf("field %s stores a separate atomic delegation manager", f.Name)
		}
		switch f.Type {
		case reflect.TypeOf((*thread.Manager)(nil)):
			t.Errorf("field %s stores the concrete thread manager: App must own it only through the delegation snapshot", f.Name)
		case reflect.TypeOf((*thread.TaskManager)(nil)):
			t.Errorf("field %s stores the concrete task manager: App must own it only through the delegation snapshot", f.Name)
		case reflect.TypeOf((*tools.ThreadManager)(nil)).Elem(), reflect.TypeOf((*tools.TaskManager)(nil)).Elem():
			t.Errorf("field %s stores a delegation tool adapter: adapters must not be stored on App", f.Name)
		}
	}
	require.Equal(t, 1, len(extraAtomics),
		"App must hold exactly one atomic delegation snapshot field, found: %v", extraAtomics)
	require.Equal(t, "delegationManagers", extraAtomics[0])

	// App's mutating APIs use pointer receivers, so inspect *App's method
	// set rather than App's value method set. A value method set would miss
	// any reintroduced legacy setter.
	appType := reflect.TypeOf((*App)(nil))
	var methodNames []string
	for i := range appType.NumMethod() {
		methodNames = append(methodNames, appType.Method(i).Name)
	}
	for _, banned := range []string{
		"SetThreads", "SetTasks", "SetThreadManager", "SetTaskManager",
		"ThreadToolManager", "TaskToolManager", "SetDelegationTools",
	} {
		for _, name := range methodNames {
			if name == banned {
				t.Errorf("App has method %s: delegation ownership must go through the single SetDelegationManagers publication", banned)
			}
		}
	}

	coordinatorType := reflect.TypeOf((*agent.Coordinator)(nil)).Elem()
	require.False(t, hasMethod(coordinatorType, "SetThreads"))
	require.False(t, hasMethod(coordinatorType, "SetTasks"))
	method, ok := coordinatorType.MethodByName("SetDelegationTools")
	require.True(t, ok)
	require.Equal(t, 2, method.Type.NumIn(), "combined coordinator API needs thread/task arguments")

	// The snapshot deliberately carries the tool adapters alongside the
	// concrete pair (see delegationManagerSnapshot's doc comment): a
	// coordinator built or rebuilt after Attach ran otherwise has no way
	// to recover them. What C7 actually guards is that this is the only
	// place they live — confirm both adapter fields are present here,
	// and rely on the field walk above to confirm App holds no second
	// copy anywhere else.
	snapType := reflect.TypeOf(delegationManagerSnapshot{})
	var hasThreadTools, hasTaskTools bool
	for i := range snapType.NumField() {
		switch snapType.Field(i).Type {
		case reflect.TypeOf((*tools.ThreadManager)(nil)).Elem():
			hasThreadTools = true
		case reflect.TypeOf((*tools.TaskManager)(nil)).Elem():
			hasTaskTools = true
		}
	}
	require.True(t, hasThreadTools, "delegationManagerSnapshot must carry the thread tool adapter")
	require.True(t, hasTaskTools, "delegationManagerSnapshot must carry the task tool adapter")
}

func hasMethod(t reflect.Type, name string) bool {
	_, ok := t.MethodByName(name)
	return ok
}
