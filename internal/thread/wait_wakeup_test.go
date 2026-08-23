package thread

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// notifyDuringGetStore fires the lifecycle's change notification from
// inside Get — that is, in the exact window between Wait reading a
// thread's status and Wait blocking on the change channel. It then
// reports the thread as finished, so a Wait that did not miss the wakeup
// returns immediately.
type notifyDuringGetStore struct {
	Store
	lc     *lifecycle
	fired  bool
	active Status
	done   Status
}

func (s *notifyDuringGetStore) Get(ctx context.Context, id string) (Thread, error) {
	st, err := s.Store.Get(ctx, id)
	if err != nil {
		return Thread{}, err
	}
	if !s.fired {
		s.fired = true
		st.Status = s.active
		// The transition lands right here, between Wait's status read
		// and its select.
		s.lc.notifyChange()
		return st, nil
	}
	st.Status = s.done
	return st, nil
}

// TestWaitDoesNotMissAChangeDuringTheStatusRead pins the ordering inside
// Manager.Wait: the change channel must be taken before the status is
// read, not after.
//
// Taken after, a transition landing in between closes a channel this
// iteration never looks at, and notifyChange installs a fresh one in its
// place — so Wait ends up waiting for the *next* transition. For a thread
// that just reached a terminal status there is no next transition, and
// Wait blocks until its own deadline: on CI that surfaced as
// TestManager_ManualPolicyCompleted failing with "context deadline
// exceeded" after a full sixty seconds, on a thread the logs showed had
// completed.
//
// The store below reproduces that interleaving deterministically rather
// than hoping a stress loop hits it.
func TestWaitDoesNotMissAChangeDuringTheStatusRead(t *testing.T) {
	t.Parallel()

	real := newTestStore(t)
	created, err := real.Create(t.Context(), testCreateParams("wakeup"))
	require.NoError(t, err)

	mgr := &Manager{}
	mgr.lc = &lifecycle{changeCh: make(chan struct{})}
	store := &notifyDuringGetStore{
		Store:  real,
		lc:     mgr.lc,
		active: StatusRunning,
		done:   StatusCompleted,
	}
	mgr.store = store
	mgr.lc.store = store

	// A budget far below anything a healthy Wait needs, so the old
	// behaviour fails fast instead of hanging out the package timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, mgr.Wait(ctx, []string{created.ID}, 2*time.Second),
		"Wait must observe a change that landed while it was reading status")
}
