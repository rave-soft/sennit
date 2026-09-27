//go:build !race

package supervisor_test

// raceDetectorEnabled mirrors the `race` build constraint the go command
// sets automatically under `-race` -- see
// internal/workspace/wsrpc/grpcws/racecheck_off_test.go and
// internal/ui/model/racecheck_off_test.go for the same pattern. This
// package's tests spawn real subprocesses, so hang-guard budgets need
// real wall-clock headroom under -race's instrumentation overhead and
// CI's cross-package CPU contention (AGENTS.md's "wall-clock budgets
// under -race"); a handful of other tests here assert something happens
// FAST relative to a production timeout, which -race's own overhead can
// blow regardless of whether the behavior under test is correct, so
// those are skipped under -race instead of widened (see
// TestEnsureRunning_TUIHoldsLock and TestEnsureRunning_ReadinessTimeout).
const raceDetectorEnabled = false
