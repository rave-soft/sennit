package model

import "time"

// raceWait widens a correctness wait/hang-guard budget under -race
// (raceDetectorEnabled, racecheck_{on,off}_test.go), the same pattern
// internal/workspace/wsrpc/grpcws uses (see its
// grpcws_test_helpers_test.go doc comment): these budgets exist only to
// turn a hang into a failure, not to assert performance, so widening
// them costs nothing but wall time on an actual hang.
func raceWait(d time.Duration) time.Duration {
	if !raceDetectorEnabled {
		return d
	}
	if w := d * 6; w > 60*time.Second {
		return w
	}
	return 60 * time.Second
}
