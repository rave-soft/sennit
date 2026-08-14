package thread

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/rave-soft/braid/internal/agent/notify"
	"github.com/rave-soft/braid/internal/pubsub"
)

// runtimeState tracks the in-memory bookkeeping for an entity whose
// workspace is currently spawned: the handle to release on removal, and
// the cancel function for its RunComplete watcher goroutine.
type runtimeState struct {
	handle      Handle
	watchCancel context.CancelFunc
	runID       string
}

// threadControl is permanent while an entity is known to the lifecycle. opMu
// serializes lifecycle operations for one entity without serializing
// unrelated entities or holding the lifecycle map lock across I/O.
type threadControl struct {
	opMu    sync.Mutex
	mu      sync.Mutex
	runtime *runtimeState
	removed bool
}

// ErrManagerClosed is returned by mutating manager operations once shutdown
// has started.
var ErrManagerClosed = errors.New("thread: manager is closed")

// runCompleteHook lets an overlay intervene when a run finishes
// successfully, before the generic lifecycle would otherwise rest the
// entity at StatusCompleted. It is called with the entity's opMu held —
// the same lock the generic StatusCompleted write it can replace would run
// under — so a hook that needs to do more work hands off to its own
// goroutine and re-acquires opMu there, exactly as the StatusCompleted
// write it replaces would have run under opMu. See [Manager.onAutoMerge]
// for the concrete case and the invariant that handoff preserves.
// Returning true means the hook has taken over the terminal transition and
// the generic path must not also record StatusCompleted; false falls
// through to the default StatusCompleted write.
type runCompleteHook func(ctx context.Context, c *threadControl, st Thread, resultText string) (handled bool)

// recoverHook lets an overlay reclassify an entity during recover before
// the generic active-status sweep runs. Returning handled=true means the
// hook has already decided st's fate (recording a terminal status of its
// own if needed) and the generic path should leave it alone.
type recoverHook func(ctx context.Context, st Thread) (handled bool, err error)

// lifecycle is the generic delegation-lifecycle machinery shared by every
// kind of background delegation this package drives: admission control,
// per-entity serialization, worker tracking, workspace release, and event
// plumbing. It has no notion of git worktrees or merge policy — those live
// in the onRunSuccess/onRecover hooks an overlay such as [Manager] supplies
// at construction, the same way it supplies a [Spawner].
type lifecycle struct {
	store        Store
	spawner      Spawner
	onRunSuccess runCompleteHook
	onRecover    recoverHook
	broker       *pubsub.Broker[Event]

	mu       sync.Mutex
	controls map[string]*threadControl
	closed   bool
	workers  sync.WaitGroup

	// changeCh is closed and replaced on every status-affecting event,
	// giving Wait a broadcast condition to select on without polling.
	changeMu sync.Mutex
	changeCh chan struct{}
}

// newLifecycle constructs a ready-to-use lifecycle backed by store and
// spawner. onRunSuccess and onRecover are optional (nil means no overlay:
// runs always rest at StatusCompleted, and recover never reclassifies
// beyond the generic active-status sweep).
func newLifecycle(store Store, spawner Spawner, onRunSuccess runCompleteHook, onRecover recoverHook) *lifecycle {
	return &lifecycle{
		store:        store,
		spawner:      spawner,
		onRunSuccess: onRunSuccess,
		onRecover:    onRecover,
		broker:       pubsub.NewBroker[Event](),
		controls:     make(map[string]*threadControl),
		changeCh:     make(chan struct{}),
	}
}

// beginOp admits one mutating operation. closeAdmission stops admission and
// the caller then waits for this count, so no operation can attach a
// runtime after teardown.
func (l *lifecycle) beginOp() (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrManagerClosed
	}
	l.workers.Add(1)
	return l.workers.Done, nil
}

func (l *lifecycle) control(id string) *threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.controls[id]
	if c == nil {
		c = &threadControl{}
		l.controls[id] = c
	}
	return c
}

func (l *lifecycle) existingControl(id string) *threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.controls[id]
}

// goWorker is called only while an admitted operation or existing worker is
// counted. Consequently its Add cannot race a shutdown's Wait after zero.
func (l *lifecycle) goWorker(fn func()) {
	l.workers.Add(1)
	go func() { defer l.workers.Done(); fn() }()
}

// subscribe returns a per-caller channel of lifecycle events.
func (l *lifecycle) subscribe(ctx context.Context) <-chan pubsub.Event[Event] {
	return l.broker.Subscribe(ctx)
}

// publish emits a lifecycle event and wakes any Wait callers blocked on a
// status change.
func (l *lifecycle) publish(t EventType, st Thread) {
	l.broker.Publish(pubsub.UpdatedEvent, Event{Type: t, Thread: st})
	l.notifyChange()
}

// setStatus is the shared SetStatus + publish helper used by every status
// transition.
func (l *lifecycle) setStatus(ctx context.Context, id string, status Status, errText, resultSummary string, completedAt int64) (Thread, error) {
	st, err := l.store.SetStatus(ctx, id, SetStatusParams{
		Status:        status,
		Error:         errText,
		ResultSummary: resultSummary,
		CompletedAt:   completedAt,
	})
	if err != nil {
		return Thread{}, err
	}
	l.publish(EventStatusChanged, st)
	return st, nil
}

func (l *lifecycle) notifyChange() {
	l.changeMu.Lock()
	close(l.changeCh)
	l.changeCh = make(chan struct{})
	l.changeMu.Unlock()
}

func (l *lifecycle) waitChan() chan struct{} {
	l.changeMu.Lock()
	defer l.changeMu.Unlock()
	return l.changeCh
}

// closeAdmission stops beginOp from admitting further work. Callers join
// the returned count (via a subsequent wait) to know once every admitted
// operation and worker goroutine it may have spawned has finished.
func (l *lifecycle) closeAdmission() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

// wait blocks until every admitted operation and worker goroutine has
// finished.
func (l *lifecycle) wait() {
	l.workers.Wait()
}

// snapshotControls returns a copy of every control known so far, letting
// callers iterate and tear entities down without holding the map lock
// across per-entity I/O.
func (l *lifecycle) snapshotControls() map[string]*threadControl {
	l.mu.Lock()
	defer l.mu.Unlock()
	controls := make(map[string]*threadControl, len(l.controls))
	for id, c := range l.controls {
		controls[id] = c
	}
	return controls
}

// handleRunComplete is the generic reaction to a RunComplete notification
// for entity id: it matches the completion to the run currently owning the
// entity's workspace, releases the workspace, and records the terminal
// outcome — interrupted on cancellation, failed on error, or (when
// onRunSuccess is unset, or declines to handle it) completed with
// rc.Text. Anything beyond that reaction — Manager's auto-merge follow-up
// is the example — is onRunSuccess's job.
func (l *lifecycle) handleRunComplete(ctx context.Context, id string, rc notify.RunComplete) {
	c := l.existingControl(id)
	if c == nil {
		return
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	rt := c.runtime
	if rt == nil || rc.RunID == "" || rc.RunID != rt.runID {
		c.mu.Unlock()
		return
	}
	c.runtime = nil
	c.mu.Unlock()
	rt.watchCancel()
	if err := l.spawner.Release(ctx, rt.handle.ID()); err != nil {
		slog.Error("thread: release completed workspace failed", "thread", id, "error", err)
	}
	st, err := l.store.Get(ctx, id)
	if err != nil {
		return
	}
	// Only react to the session this entity currently owns, and only
	// while a run is actually in flight: Remove or a completed merge can
	// race a straggling RunComplete from a run that no longer matters.
	if rc.SessionID != st.SessionID || st.Status != StatusRunning {
		return
	}

	if rc.Cancelled {
		if _, err := l.setStatus(ctx, id, StatusInterrupted, "", "", 0); err != nil {
			slog.Error("thread: recording run cancellation failed", "thread", id, "error", err)
		}
		return
	}
	if rc.Error != "" {
		if _, err := l.setStatus(ctx, id, StatusFailed, rc.Error, "", 0); err != nil {
			slog.Error("thread: recording run failure failed", "thread", id, "error", err)
		}
		return
	}

	if l.onRunSuccess != nil && l.onRunSuccess(ctx, c, st, rc.Text) {
		return
	}

	if _, err := l.setStatus(ctx, id, StatusCompleted, "", rc.Text, time.Now().Unix()); err != nil {
		slog.Error("thread: recording run completion failed", "thread", id, "error", err)
	}
}

// recover reconciles store state against reality after a process restart:
// entities left in an active status (their goroutines are gone with the
// old process) become interrupted. Before that generic sweep runs,
// onRecover (if set) gets a chance to reclassify st itself — Manager's
// overlay uses it to fail entities whose worktree has vanished from disk,
// a check the generic path has no business making.
//
// This sweeps every delegation kind (ListAll), not just threads: it is
// the generic path, and a kind-scoped listing here would leave any other
// kind's interrupted runs silently unreconciled after a restart — exactly
// the "left displayed as running forever" state recovery exists to
// prevent.
func (l *lifecycle) recover(ctx context.Context) error {
	threads, err := l.store.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, st := range threads {
		if l.onRecover != nil {
			handled, err := l.onRecover(ctx, st)
			if err != nil {
				return err
			}
			if handled {
				continue
			}
		}
		if st.Status.Active() {
			if _, err := l.setStatus(ctx, st.ID, StatusInterrupted, "", "", 0); err != nil {
				return err
			}
		}
	}
	return nil
}
