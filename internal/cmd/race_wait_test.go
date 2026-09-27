package cmd

import "time"

// raceWait widens a correctness wait/hang-guard budget under -race, the
// same pattern internal/workspace/wsrpc/grpcws and internal/daemon use
// (see grpcws_test_helpers_test.go's doc comment). These budgets exist
// only to turn a hang into a failure, not to assert performance, so
// widening them costs nothing but wall time on an actual hang.
func raceWait(d time.Duration) time.Duration {
	if !raceDetectorEnabled {
		return d
	}
	if w := d * 6; w > 60*time.Second {
		return w
	}
	return 60 * time.Second
}
