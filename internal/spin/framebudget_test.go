package spin

import (
	"testing"
	"time"
)

// resetFrameBudget clears the package-level reservation cursor so a test
// starts from a quiet clock rather than inheriting another test's queue.
func resetFrameBudget() {
	frameSlotMu.Lock()
	nextFrameSlot = time.Time{}
	frameSlotMu.Unlock()
}

// TestReserveFrameSlot_KeepsNaturalCadenceWhenQuiet pins the single-anim
// case: one chain is far below the aggregate ceiling, so its ticks must
// not be delayed at all.
func TestReserveFrameSlot_KeepsNaturalCadenceWhenQuiet(t *testing.T) {
	resetFrameBudget()

	for range 10 {
		got := reserveFrameSlot()
		if got < frameInterval-5*time.Millisecond || got > frameInterval+5*time.Millisecond {
			t.Fatalf("a lone chain should keep its natural %v cadence, got %v", frameInterval, got)
		}
		// A lone chain reserves its next slot only after the previous one
		// has fired, which is what keeps it below the ceiling.
		frameSlotMu.Lock()
		nextFrameSlot = time.Now()
		frameSlotMu.Unlock()
	}
}

// TestReserveFrameSlot_SpacesManyChains is the point of the budget: with
// enough chains live at once, their ticks are spread instead of all
// arriving within the same natural interval and each costing a full
// re-render.
func TestReserveFrameSlot_SpacesManyChains(t *testing.T) {
	resetFrameBudget()

	const chains = 40

	var delays []time.Duration
	for range chains {
		delays = append(delays, reserveFrameSlot())
	}

	// Unbudgeted, all 40 would fire within one natural interval. Budgeted,
	// the last one is pushed out to roughly chains/maxAggregateFPS seconds
	// — clamped by maxFrameDelay.
	last := delays[len(delays)-1]
	if last <= frameInterval {
		t.Fatalf("with %d live chains the last tick should be pushed past %v, got %v", chains, frameInterval, last)
	}
	if last > maxFrameDelay+5*time.Millisecond {
		t.Fatalf("no tick may be delayed past %v, got %v", maxFrameDelay, last)
	}

	// Spacing between consecutive reservations must not exceed the
	// ceiling's period, or the budget would be starving the animation
	// rather than pacing it.
	spacing := time.Second / maxAggregateFPS
	for i := 1; i < len(delays); i++ {
		if gap := delays[i] - delays[i-1]; gap > spacing+5*time.Millisecond {
			t.Fatalf("reservation %d is %v after its predecessor, want at most %v", i, gap, spacing)
		}
	}
}

// TestReserveFrameSlot_NeverGoesBackwards guards the clamp: a long
// backlog must not hand a caller a negative or zero delay, which would
// turn its tick chain into a busy loop.
func TestReserveFrameSlot_NeverGoesBackwards(t *testing.T) {
	resetFrameBudget()

	for i := range 500 {
		if got := reserveFrameSlot(); got <= 0 {
			t.Fatalf("reservation %d returned a non-positive delay %v", i, got)
		}
	}
}
