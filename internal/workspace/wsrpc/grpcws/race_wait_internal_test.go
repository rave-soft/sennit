package grpcws

import "time"

// raceWait mirrors the grpcws_test package's own raceWait (see
// grpcws_test_helpers_test.go's doc comment) for this package's internal
// tests: it widens a correctness wait's timeout under -race, where
// cross-package CPU contention makes a budget that comfortably clears in
// isolation arrive late under CI's race job (see AGENTS.md's "wall-clock
// budgets under -race"). Leave a budget alone when it is itself a
// performance assertion.
func raceWait(d time.Duration) time.Duration {
	if !raceDetectorEnabledInternal {
		return d
	}
	if w := d * raceMultiplierInternal; w > raceFloorInternal {
		return w
	}
	return raceFloorInternal
}

const (
	raceMultiplierInternal = 6
	raceFloorInternal      = 60 * time.Second
)
