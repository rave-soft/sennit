package app

import (
	"sync"
	"testing"
	"time"

	"github.com/rave-soft/sennit/internal/agent"
	"github.com/stretchr/testify/require"
)

// raceTestCoordinator is a minimal agent.Coordinator used only to give
// setCoordinator something non-nil to install; none of its methods are
// exercised.
type raceTestCoordinator struct {
	agent.Coordinator
}

// TestCoordinator_ConcurrentReadWriteIsRaceFree hammers Coordinator() from
// several reader goroutines while SetAgentCoordinatorForTest swaps the
// coordinator from another, mirroring the real contention between
// AppWorkspace's request-handling methods and initCoderAgent/
// SetDelegationManagers. Before agentCoordinatorMu existed, the equivalent
// unsynchronized field access failed `go test -race` for this exact
// pattern; this test exists to keep it that way.
func TestCoordinator_ConcurrentReadWriteIsRaceFree(t *testing.T) {
	a := NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	// A closed channel, not time.After's channel, so every goroutine's
	// select observes it: time.After only ever delivers one value, which
	// only the first goroutine to reach its select would consume, leaving
	// the rest spinning in their default case forever.
	stop := make(chan struct{})
	time.AfterFunc(100*time.Millisecond, func() { close(stop) })
	var wg sync.WaitGroup

	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = a.Coordinator()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				a.SetAgentCoordinatorForTest(&raceTestCoordinator{})
			}
		}
	}()

	wg.Wait()
}

// TestCoordinator_NilBeforeInitCoderAgent proves an App with no coordinator
// installed yet reports nil rather than panicking, and that a typed-nil
// concrete pointer stored into the interface (never a case any production
// write site hits, but worth pinning against) is not what production code
// stores — Coordinator() must answer plain nil for the "not ready" case.
func TestCoordinator_NilBeforeInitCoderAgent(t *testing.T) {
	a := NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)

	require.Nil(t, a.Coordinator())
}
